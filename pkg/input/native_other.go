//go:build !windows

package input

import (
	"context"
	"io"
	"time"
)

func openNative(context.Context, io.Writer) (source, error) { return nil, ErrUnavailable }
func eventTime(uint64) time.Time                            { return time.Now() }
