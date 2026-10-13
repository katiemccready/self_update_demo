package update

import "selfupdate/internal/update/install"

const (
	RestartExitCode     = install.RestartExitCode
	EnvironmentExitCode = install.EnvironmentExitCode
)

func (u *Updater) Confirm() error { return u.store.Confirm(u.cur) }

func (u *Updater) ReportBadRelease(reason error) (rolled bool, err error) {
	return u.store.ReportBad(u.cur, u.entry, reason)
}

func (u *Updater) Restart() { u.exitFn(RestartExitCode) }
