//go:build windows

package main

import "golang.org/x/sys/windows"

func detectEastAsianPlatform() bool {
	cp, _ := windows.GetConsoleOutputCP()
	return cp == 932 || cp == 936 || cp == 949 || cp == 950
}
