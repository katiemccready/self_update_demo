package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"selfupdate/internal/fsutil"
	"selfupdate/internal/pack"
	"selfupdate/internal/release"
	"selfupdate/internal/update/install"
	"selfupdate/internal/update/manifest"
)

const usageText = `usage:
  updatectl keygen    -out DIR
  updatectl pack      -dir DIR -entry PATH -out FILE.tar.gz
                         (deterministic package of a program directory; PATH is the file the launcher starts)
  updatectl publish   -key FILE -root DIR -version X.Y.Z -os OS -arch ARCH (-dir DIR | -package FILE) -entry PATH
                         [-channel stable] [-name agent] [-notes TEXT] [-mandatory] [-valid-for 720h] [-force] [-rollout PERCENT]
  updatectl rollout   -root DIR -os OS -arch ARCH [-channel stable] (-percent N | -halt | -resume) [-note TEXT]
                         (no key needed: widen, halt or resume the latest release's staged rollout;
                          only cmd/updateserver honours it)
  updatectl refresh   -key FILE -root DIR -os OS -arch ARCH [-channel stable] [-valid-for 720h]
  updatectl bootstrap -root DIR -launcher FILE -package FILE -version X.Y.Z -entry PATH
                         (first install on a machine: lays out DIR as an install root; run DIR/launcher afterwards)

Manifests expire (default 30 days) and clients refuse expired ones, so run
"refresh" on a schedule shorter than -valid-for for every published target.`

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "publish":
		err = publish(os.Args[2:])
	case "rollout":
		err = rollout(os.Args[2:])
	case "refresh":
		err = refresh(os.Args[2:])
	case "pack":
		err = packCmd(os.Args[2:])
	case "bootstrap":
		err = bootstrap(os.Args[2:])
	default:
		usage()
	}
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, usageText)
	os.Exit(2)
}

func requireFlags(fs *flag.FlagSet, names ...string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, n := range names {
		if !set[n] || fs.Lookup(n).Value.String() == "" {
			return fmt.Errorf("-%s is required", n)
		}
	}
	return nil
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", ".", "directory for private.key / public.key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	privPath, pubPath := filepath.Join(*out, "private.key"), filepath.Join(*out, "public.key")

	for _, p := range []string{privPath, pubPath} {
		if _, err := os.Lstat(p); err == nil {
			return errKeyExists(p)
		}
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString
	if err := writeNew(privPath, b64(priv.Seed()), 0o600); err != nil {
		return err
	}
	if err := writeNew(pubPath, b64(pub), 0o644); err != nil {
		_ = os.Remove(privPath)
		return err
	}
	fmt.Printf("wrote %s (keep secret, never serve it) and %s\npublic key: %s\n", privPath, pubPath, b64(pub))
	return nil
}

func errKeyExists(path string) error {
	return fmt.Errorf("%s already exists; refusing to overwrite (move it away deliberately if you really mean to rotate keys)", path)
}

func writeNew(path, content string, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, os.ErrExist) {
		return errKeyExists(path)
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

func readKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: not a valid private key", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

type targetFlags struct {
	key, root, channel, goos, goarch *string
	validFor                         *time.Duration
}

func addTargetFlags(fs *flag.FlagSet, validForUsage string) targetFlags {
	return targetFlags{
		key:      fs.String("key", "", "private key file"),
		root:     fs.String("root", "releases", "release tree root"),
		channel:  fs.String("channel", "stable", "release channel"),
		goos:     fs.String("os", "", "target GOOS"),
		goarch:   fs.String("arch", "", "target GOARCH"),
		validFor: fs.Duration("valid-for", release.DefaultValidity, validForUsage),
	}
}

func (t targetFlags) parse(fs *flag.FlagSet, args []string, required ...string) (ed25519.PrivateKey, error) {
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := requireFlags(fs, required...); err != nil {
		return nil, err
	}
	return readKey(*t.key)
}

func publish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	t := addTargetFlags(fs, "how long clients may trust this manifest before it must be refreshed")
	version := fs.String("version", "", "version X.Y.Z")
	dir := fs.String("dir", "", "program directory to package and publish")
	pkgPath := fs.String("package", "", "ready-made .tar.gz to publish (alternative to -dir)")
	entry := fs.String("entry", "", "program inside the package that the launcher starts, e.g. agent or bin/app.exe")
	name := fs.String("name", "agent", "published package name (NAME.tar.gz)")
	notes := fs.String("notes", "", "release notes shown to users")
	mandatory := fs.Bool("mandatory", false, "mark as mandatory (skips user prompts; e.g. security fixes)")
	force := fs.Bool("force", false, "allow a non-increasing version (never overwrites a published package with different contents)")
	staged := fs.Int("rollout", -1, "offer the release to only this percent (0-100) of clients at first; widen with `rollout`")
	key, err := t.parse(fs, args, "key", "version", "os", "arch", "entry")
	if err != nil {
		return err
	}
	if (*dir == "") == (*pkgPath == "") {
		return errors.New("give exactly one of -dir and -package")
	}
	if *dir != "" {
		tmp, err := os.MkdirTemp("", "updatectl-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		*pkgPath = filepath.Join(tmp, *name+release.PackageExt)
		if err := buildPackage(*dir, *entry, *pkgPath); err != nil {
			return err
		}
	}
	var stagedPercent *int
	if *staged >= 0 {
		stagedPercent = staged
	}
	m, err := release.Publish(release.Options{
		StagedPercent: stagedPercent,
		Root:          *t.root, Channel: *t.channel, Version: *version, OS: *t.goos, Arch: *t.goarch,
		PackagePath: *pkgPath, Entry: *entry, Name: *name, Key: key, Notes: *notes, Mandatory: *mandatory,
		Validity: *t.validFor, Force: *force,
	})
	if err != nil {
		return err
	}
	fmt.Printf("published %s %s/%s_%s sha256=%s size=%d expires=%s\n",
		m.Version, m.Channel, m.OS, m.Arch, m.SHA256, m.Size, m.Expires.Format(time.RFC3339))
	if stagedPercent != nil {
		fmt.Printf("staged: offered to %d%% of clients. This is enforced by updateserver only; "+
			"a plain static host (nginx, S3) serves every client the new release.\n", *stagedPercent)
	}
	return nil
}

func buildPackage(dir, entry, out string) error {
	var buf bytes.Buffer
	if err := pack.Build(dir, entry, &buf); err != nil {
		return err
	}
	return fsutil.IgnoreDirSync(fsutil.WriteFileAtomic(out, buf.Bytes(), 0o644))
}

func packCmd(args []string) error {
	fs := flag.NewFlagSet("pack", flag.ContinueOnError)
	dir := fs.String("dir", "", "program directory")
	entry := fs.String("entry", "", "program inside it that the launcher starts")
	out := fs.String("out", "", "package file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireFlags(fs, "dir", "entry", "out"); err != nil {
		return err
	}
	if err := buildPackage(*dir, *entry, *out); err != nil {
		return err
	}
	sum, size, err := fsutil.HashFile(*out)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s sha256=%s size=%d\n", *out, sum, size)
	return nil
}

func bootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	root := fs.String("root", "", "install root to create (must not already be one)")
	launcherPath := fs.String("launcher", "", "launcher binary for this OS and architecture")
	pkgPath := fs.String("package", "", "package of the first version")
	version := fs.String("version", "", "version of that package")
	entry := fs.String("entry", "", "program inside the package that the launcher starts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireFlags(fs, "root", "launcher", "package", "version", "entry"); err != nil {
		return err
	}
	v, err := manifest.ParseVersion(*version)
	if err != nil {
		return err
	}
	f, err := os.Open(*pkgPath)
	if err != nil {
		return err
	}
	defer f.Close()
	r := install.Root{Dir: *root}
	if err := install.Bootstrap(r, *launcherPath, f, v, *entry); err != nil {
		return err
	}
	fmt.Printf("installed %s into %s; start it with %s\n", v, *root, r.Launcher())
	return nil
}

func rollout(args []string) error {
	fs := flag.NewFlagSet("rollout", flag.ContinueOnError)
	root := fs.String("root", "releases", "release tree root")
	channel := fs.String("channel", "stable", "release channel")
	goos := fs.String("os", "", "target GOOS")
	goarch := fs.String("arch", "", "target GOARCH")
	percent := fs.Int("percent", -1, "offer the latest release to this percent (0-100) of clients")
	halt := fs.Bool("halt", false, "stop offering the latest release to anyone")
	resume := fs.Bool("resume", false, "lift a halt")
	note := fs.String("note", "", "why (recorded in rollout.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireFlags(fs, "os", "arch"); err != nil {
		return err
	}
	o := release.RolloutOptions{Root: *root, Channel: *channel, OS: *goos, Arch: *goarch,
		Halt: *halt, Resume: *resume, Note: *note}
	if *percent >= 0 {
		o.Percent = percent
	}
	r, err := release.SetRollout(o)
	if err != nil {
		return err
	}
	state := fmt.Sprintf("%d%%", r.Percent)
	if r.Halt {
		state = "HALTED (was " + state + ")"
	}
	fmt.Printf("rollout of %s %s/%s_%s: %s\n", r.Version, *channel, *goos, *goarch, state)
	return nil
}

func refresh(args []string) error {
	fs := flag.NewFlagSet("refresh", flag.ContinueOnError)
	t := addTargetFlags(fs, "new validity window")
	key, err := t.parse(fs, args, "key", "os", "arch")
	if err != nil {
		return err
	}
	m, err := release.Refresh(release.RefreshOptions{
		Root: *t.root, Channel: *t.channel, OS: *t.goos, Arch: *t.goarch, Key: key, Validity: *t.validFor,
	})
	if err != nil {
		return err
	}
	fmt.Printf("refreshed %s %s/%s_%s expires=%s\n", m.Version, m.Channel, m.OS, m.Arch, m.Expires.Format(time.RFC3339))
	return nil
}
