//go:build windows || linux

package clipboard

import (
	"context"
	"time"
)

type pollableProvider interface {
	ReadText(ctx context.Context) ([]byte, error)
}

func fallbackPoll(ctx context.Context, p pollableProvider, ch chan<- struct{}) {
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		var lastHash string
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				text, err := p.ReadText(ctx)
				if err != nil {
					continue
				}
				h := string(text)
				if h != lastHash {
					lastHash = h
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
}
