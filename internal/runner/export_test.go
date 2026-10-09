package runner

import "time"

// SessionStartTimeout reports the shipped bound, so a test can pin the value
// and not only the mechanism.
func SessionStartTimeout() time.Duration { return sessionStartTimeout }

// SetSessionStartTimeout shrinks the SessionStart bound for a test and restores
// it afterwards. Tests in this package do not run in parallel, which is what
// makes writing a package-level variable safe here.
func SetSessionStartTimeout(d time.Duration) func() {
	old := sessionStartTimeout
	sessionStartTimeout = d
	return func() { sessionStartTimeout = old }
}

// PauseBetweenBuildAndPublish widens the window in which a run's view has been
// built and not yet sent, so a test can land a Remove inside it. Tests in this
// package do not run in parallel, which is what makes writing a package-level
// variable safe here.
func PauseBetweenBuildAndPublish(during func()) func() {
	betweenBuildAndPublish = during
	return func() { betweenBuildAndPublish = nil }
}

// CloseRun ends a run's session and keeps its record, which is the state a run
// reaches after a restart. Tests use it to get a closed run without one.
func (r *Runs) CloseRun(id string) error {
	r.mu.Lock()
	lr := r.byID[id]
	if lr == nil {
		r.mu.Unlock()
		return ErrNoRun
	}
	sess, rel := lr.sess, lr.release
	lr.sess, lr.release = nil, nil
	r.mu.Unlock()
	if sess == nil {
		return nil
	}
	meta := sess.Local.Meta()

	r.mu.Lock()
	r.markClosedLocked(lr, meta)
	lr.version++
	r.saveLocked()
	rec := lr.rec
	r.mu.Unlock()

	r.notify(view(rec, nil))
	closeSession(sess, rel)
	return nil
}
