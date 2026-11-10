package botsupervisor

// UnsafeTestBuild reports whether this binary was built with the dilla_unsafe_test tag (unsafe_on.go). A release
// never is.
func UnsafeTestBuild() bool { return unsafeTestBuild }
