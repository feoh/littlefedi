// Package auth provides password hashing and verification helpers.
package auth

import (
	"golang.org/x/crypto/bcrypt"
)

const DefaultCost = bcrypt.DefaultCost // 10

func HashPassword(plaintext string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(plaintext), DefaultCost)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

func CheckPassword(plaintext, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext))
	return err == nil
}
