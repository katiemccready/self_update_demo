//go:build windows

package launcher

import "selfupdate/internal/winjob"

var treeJob *winjob.Job

func KillTreeOnExit() error {
	j, err := winjob.New()
	if err != nil {
		return err
	}
	if err := j.AssignSelf(); err != nil {
		j.Close()
		return err
	}
	treeJob = j
	return nil
}
