//go:build !postgres

package streaming

import "fmt"

func newPostgresBroker(string) (Broker, error) {
	return nil, fmt.Errorf("streaming: postgres broker requires a binary built with -tags postgres")
}
