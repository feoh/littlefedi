package main

import (
	"golang.org/x/term"
)

func getTermios(fd int) (interface{}, error) {
	return term.GetState(fd)
}

func readTerminalPassword(fd int) (string, error) {
	b, err := term.ReadPassword(fd)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
