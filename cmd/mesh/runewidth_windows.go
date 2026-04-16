//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func detectEastAsianPlatform() bool {
	// Windows Terminal renders EA Ambiguous chars as 2-wide with most fonts.
	if os.Getenv("WT_SESSION") != "" {
		return true
	}
	cp, _ := windows.GetConsoleOutputCP()
	return cp == 932 || cp == 936 || cp == 949 || cp == 950
}
