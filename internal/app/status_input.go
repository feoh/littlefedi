package app

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ValidateStatusInput is shared by immediate and scheduled publication.
func ValidateStatusInput(input StatusInput) error {
	input.Content = strings.TrimSpace(input.Content)
	if input.Visibility != "" && !validVisibility(input.Visibility) {
		return fmt.Errorf("invalid visibility")
	}
	if input.Content == "" && len(input.MediaIDs) == 0 && len(input.PollOptions) == 0 {
		return fmt.Errorf("status is empty")
	}
	if len(input.PollOptions) > 0 {
		if len(input.PollOptions) < 2 || len(input.PollOptions) > 4 {
			return fmt.Errorf("poll must have 2 to 4 options")
		}
		if input.PollExpiresIn < 5*time.Minute || input.PollExpiresIn > 30*24*time.Hour {
			return fmt.Errorf("poll expiration out of range")
		}
		for _, o := range input.PollOptions {
			if n := utf8.RuneCountInString(strings.TrimSpace(o)); n == 0 || n > 50 {
				return fmt.Errorf("invalid poll option")
			}
		}
	}
	return nil
}
