// Copyright (c) Contributors to the Apptainer project, established as
//   Apptainer a Series of LF Projects LLC.
//   For website terms of use, trademark policy, privacy policy and other
//   project policies see https://lfprojects.org/policies
// Copyright (c) 2018-2019, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package apptainer

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/apptainer/apptainer/internal/pkg/plugin"
	apptainercallback "github.com/apptainer/apptainer/pkg/plugin/callback/runtime/engine/apptainer"
	"github.com/apptainer/apptainer/pkg/sylog"
	"golang.org/x/sys/unix"
)

// MonitorContainer is called from master once the container has
// been spawned. It will block until the container exists.
//
// Additional privileges may be gained when running
// in suid flow. However, when a user namespace is requested and it is not
// a hybrid workflow (e.g. fakeroot), then there is no privileged saved uid
// and thus no additional privileges can be gained.
//
// Particularly here no additional privileges are gained as monitor does
// not need them for wait4 and kill syscalls.
func (e *EngineOperations) MonitorContainer(pid int, signals chan os.Signal) (syscall.WaitStatus, error) {
	var status syscall.WaitStatus

	callbackType := (apptainercallback.MonitorContainer)(nil)
	callbacks, err := plugin.LoadCallbacks(callbackType)
	if err != nil {
		return status, fmt.Errorf("while loading plugins callbacks '%T': %s", callbackType, err)
	}
	if len(callbacks) > 1 {
		return status, fmt.Errorf("multiple plugins have registered callback for '%T'", callbackType)
	} else if len(callbacks) == 1 {
		return callbacks[0].(apptainercallback.MonitorContainer)(e.CommonConfig, pid, signals)
	}

	// If we depend only on syscall.SIGCHLD to find out when a child dies,
	// we sometimes do not see the signal until some other signal is
	// received.  This problem has been seen in particular in setuid mode
	// on Ubuntu 22.04 with go1.20.5 when one of the image driver programs
	// dies with an immediate error.  To avoid that problem, use waitid
	// with WNOWAIT in a separate go func and ignore the SIGCHLD signal.

	childExited := make(chan error, 1)

	go func() {
		for {
			var siginfo unix.Siginfo
			err := unix.Waitid(unix.P_ALL, 0, &siginfo, syscall.WNOWAIT|syscall.WEXITED, nil)
			if err == syscall.ECHILD {
				break
			}
			if err != syscall.EINTR {
				childExited <- err
				// In case it is not a process known to the
				// Monitor, give a chance for another thread
				// to identify it
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	for {
		select {
		case err := <-childExited:
			if err != nil {
				return status, fmt.Errorf("error from Waitid while waiting for child: %s", err)
			}
			var wpid int
			wpid, err = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
			if err != nil {
				return status, fmt.Errorf("error while waiting for child %v: %s", pid, err)
			}
			if wpid == pid {
				sylog.Debugf("Container process stopped")
				return status, nil
			}
			if imageDriver != nil {
				status, err = imageDriver.CheckStopped()
				if err != nil {
					return status, err
				}
			}
			sylog.Debugf("Stopped child not identified")
			continue
		case s := <-signals:
			switch s {
			case syscall.SIGCHLD:
				// Ignore SIGCHLD because it is handled by separate go func
				continue
			case syscall.SIGURG:
				// Ignore SIGURG, which is used for non-cooperative goroutine
				// preemption starting with Go 1.14. For more information, see
				// https://github.com/golang/go/issues/24543.
				break
			default:
				sylog.Debugf("%s received", s)
				if e.EngineConfig.GetSignalPropagation() {
					if err := syscall.Kill(pid, s.(syscall.Signal)); err != nil {
						return status, fmt.Errorf("interrupted by signal %s", s.String())
					}
				}
				// Handle CTRL-Z and send ourself a SIGSTOP to implicitly send SIGCHLD
				// signal to parent process as this process is the direct child
				if s == syscall.SIGTSTP {
					if err := syscall.Kill(os.Getpid(), syscall.SIGSTOP); err != nil {
						return status, fmt.Errorf("received SIGTSTP but was not able to stop")
					}
				}
			}
		}
	}
}
