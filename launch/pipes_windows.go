//go:build windows

package launch

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/xattribution/gohelm/cdp"
	"github.com/xattribution/gohelm/cdp/pipeconn"
)

// startWithPipes passes the browser's ends of two pipes as inherited
// handles named by --remote-debugging-io-pipes=<read>,<write>. Go limits
// inheritance to exactly those two handles (PROC_THREAD_ATTRIBUTE_HANDLE_LIST),
// so nothing else of this program leaks into the browser.
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
	hr, hw := syscall.Handle(inR.Fd()), syscall.Handle(outW.Fd())
	for _, h := range []syscall.Handle{hr, hw} {
		if err := syscall.SetHandleInformation(h, syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
			inR.Close()
			inW.Close()
			outR.Close()
			outW.Close()
			return nil, err
		}
	}
	cmd.Args = append(cmd.Args[:len(cmd.Args)-1], fmt.Sprintf("--remote-debugging-io-pipes=%d,%d", uintptr(hr), uintptr(hw)), cmd.Args[len(cmd.Args)-1])
	cmd.SysProcAttr = &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{hr, hw}}
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
