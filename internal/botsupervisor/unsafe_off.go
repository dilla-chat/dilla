//go:build !dilla_unsafe_test

package botsupervisor

// unsafeTestBuild is false in every build but one made with the dilla_unsafe_test tag (unsafe_on.go): the relaxed
// branches it guards compile out.
const unsafeTestBuild = false
