//go:build !s3

package objectstore

import (
	"brew.bsd.cafe/littlecomputerpeople/littlefedi/config"
	"fmt"
)

func newS3(config.Media, bool) (Store, error) {
	return nil, fmt.Errorf("S3 media support is not compiled in; rebuild littleFedi with -tags s3")
}
