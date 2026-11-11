// Package cli holds the shared pieces of the pocketbox commands: the runtime
// selection, log streaming, and the HTTP daemon.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pocketbox/pocketbox/pkg/dirs"
	"github.com/pocketbox/pocketbox/pkg/engine"
	"github.com/pocketbox/pocketbox/pkg/images"
	"github.com/pocketbox/pocketbox/pkg/netg"
)

// RuntimeName describes where containers execute.
func RuntimeName() string {
	if inGuest() {
		return "linux guest (pocketbox runtime)"
	}
	if runtime.GOOS == "linux" {
		return "native linux"
	}
	return runtime.GOOS
}

func inGuest() bool {
	return os.Getenv("POCKETBOX_GUEST") != "" || strings.HasPrefix(os.Getenv("POCKETBOX_DATA"), "/pb")
}

// AttachHostBridge points image downloads at the runtime host when the guest
// has no network of its own.
func AttachHostBridge(imgs *images.Store, l dirs.Layout) {
	if !inGuest() {
		return
	}
	_ = netg.DirName
	imgs.SetOptions(imagesOptions(l.Root + "/" + netg.DirName))
}

// PrintJSON writes v as indented JSON.
func PrintJSON(w io.Writer, v interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Run creates and starts a container, streaming logs when attached.
func Run(e *engine.Engine, opts engine.CreateOptions, detach, interactive bool, out io.Writer) (int, error) {
	c, err := e.Create(opts)
	if err != nil {
		return 1, err
	}
	if err := e.Start(c.ID); err != nil {
		return 1, err
	}
	if detach {
		fmt.Fprintln(out, c.ID)
		return 0, nil
	}
	// Attached: follow the container's log file until it exits.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_ = Follow(e, c, 0, out, stop)
		close(done)
	}()
	code, _ := e.Wait(c.ID)
	close(stop)
	<-done
	if opts.AutoRemove {
		_ = e.Remove(c.ID, true)
	}
	fmt.Fprintf(out, "\n[exited with code %d]\n", code)
	return code, nil
}

// Logs prints a container's log. With follow it keeps streaming until the
// process exits.
func Logs(e *engine.Engine, c *engine.Container, tail int, follow bool, out io.Writer) (int, error) {
	if follow {
		stop := make(chan struct{})
		go func() {
			if _, err := e.Wait(c.ID); err == nil {
				close(stop)
			}
		}()
		return 0, Follow(e, c, tail, out, stop)
	}
	data, err := os.ReadFile(e.LogPath(c.ID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 1, err
	}
	if tail > 0 {
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) > tail {
			lines = lines[len(lines)-tail:]
		}
		data = []byte(strings.Join(lines, "\n") + "\n")
	}
	_, err = out.Write(data)
	return 0, err
}

// Follow copies the log file offset 0 (or its tail) to out until stop closes.
func Follow(e *engine.Engine, c *engine.Container, tail int, out io.Writer, stop <-chan struct{}) error {
	f, err := e.LogsFile(c.ID)
	if err != nil {
		return err
	}
	defer f.Close()
	if tail > 0 {
		data, _ := io.ReadAll(f)
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) > tail {
			lines = lines[len(lines)-tail:]
			_, _ = out.Write([]byte(strings.Join(lines, "\n") + "\n"))
		} else {
			_, _ = out.Write(data)
		}
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-stop:
			// Drain what is left, then return.
			for {
				n, err := f.Read(buf)
				if n > 0 {
					_, _ = out.Write(buf[:n])
				}
				if err != nil {
					return nil
				}
			}
		case <-ticker.C:
			for {
				n, err := f.Read(buf)
				if n > 0 {
					_, _ = out.Write(buf[:n])
				}
				if err != nil {
					break
				}
			}
		}
	}
}
