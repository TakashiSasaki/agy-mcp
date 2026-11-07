package testutil

import (
	"errors"
	"syscall"
)

func groupHasLiveMembers(pgid int) (bool, error) {
	err := syscall.Kill(-pgid, 0)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return false, nil
	}
	return err == nil, err
}

func ObserveProcess(pid int) (func() (bool, error), error) {
	if err := syscall.Kill(pid, 0); err != nil {
		return nil, err
	}
	return func() (bool, error) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return err == nil, err
	}, nil
}
