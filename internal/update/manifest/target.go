package manifest

import (
	"fmt"
	"regexp"
)

const FileName = "manifest.json"

const MaxBytes = 1 << 20

const ClientIDHeader = "X-Client-ID"

var (
	channelRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	platformRe = regexp.MustCompile(`^[a-z0-9]+$`)
)

func ValidateChannel(s string) error {
	if !channelRe.MatchString(s) {
		return fmt.Errorf("invalid channel %q", s)
	}
	return nil
}

func ValidatePlatform(goos, goarch string) error {
	if !platformRe.MatchString(goos) || !platformRe.MatchString(goarch) {
		return fmt.Errorf("invalid os/arch %q/%q", goos, goarch)
	}
	return nil
}

func TargetPath(channel, goos, goarch string) string {
	return channel + "/" + goos + "_" + goarch
}

func Path(channel, goos, goarch string) string {
	return TargetPath(channel, goos, goarch) + "/" + FileName
}
