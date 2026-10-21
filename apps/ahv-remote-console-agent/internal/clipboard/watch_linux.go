//go:build linux

package clipboard

import (
	"bufio"
	"context"
	"os/exec"
)

func (p *linuxProvider) Watch(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)

	if len(p.readCmd) > 0 && p.readCmd[0] == "wl-paste" {
		go func() {
			for {
				if ctx.Err() != nil {
					return
				}
				// --watch runs as a permanent daemon. It spawns 'echo change' for every clipboard event.
				// This replaces the 250ms polling loop and eliminates Wayland compositor connection churn.
				cmd := exec.CommandContext(ctx, "wl-paste", "--type", "text/plain", "--watch", "echo", "change")
				cmd.Env = sanitizedEnv()
				
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					fallbackPoll(ctx, p, ch)
					return
				}
				
				if err := cmd.Start(); err != nil {
					fallbackPoll(ctx, p, ch)
					return
				}

				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					select {
					case ch <- struct{}{}:
					default:
					}
				}
				_ = cmd.Wait()
			}
		}()
	} else {
		fallbackPoll(ctx, p, ch)
	}

	return ch
}
