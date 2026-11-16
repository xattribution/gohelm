//go:build !windows

package launch

import (
	"os"
	"os/exec"

	"github.com/xattribution/gohelm/cdp"
	"github.com/xattribution/gohelm/cdp/pipeconn"
)

// startWithPipes gives the browser its input pipe as fd 3 and its output
// pipe as fd 4, as --remote-debugging-pipe expects.
func startWithPipes(cmd *exec.Cmd) (cdp.MessageConn, error) {
	inR, inW, err := os.Pipe() // we write, the browser reads
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe() // the browser writes, we read
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{inR, outW}
	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	inR.Close()
	outW.Close()
	return pipeconn.New(outR, inW, func() error {
		inW.Close()
		return outR.Close()
	}), nil
}
