//go:build !include_plugin_script

package main

import "errors"

func scriptSlotDir(string) (string, error) {
	return "", errors.New("this binary was built without the script plugin")
}

func scriptFingerprint([]string) string { return "" }
