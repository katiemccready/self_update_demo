package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	greetingFile = "data/greeting"
	engineFile   = "lib/engine.dat"
)

func programDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func loadAssets(dir, want string) (greeting string, err error) {
	read := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("program file %s: %w", rel, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	greeting, err = read(greetingFile)
	if err != nil {
		return "", err
	}
	engine, err := read(engineFile)
	if err != nil {
		return "", err
	}
	if greeting != "hello "+want || engine != "engine "+want {
		return "", fmt.Errorf("program files do not belong to version %s (greeting %q, engine %q)", want, greeting, engine)
	}
	return greeting, nil
}
