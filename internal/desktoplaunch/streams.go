package desktoplaunch

import "errors"

type dataStreamPolicy struct{ directory, defaultSeen bool }

func (p *dataStreamPolicy) add(name string) error {
	if p.directory || name != "::$DATA" || p.defaultSeen {
		return errors.New("unexpected named or duplicate program data stream")
	}
	p.defaultSeen = true
	return nil
}
func (p *dataStreamPolicy) finish() error {
	if !p.directory && !p.defaultSeen {
		return errors.New("missing default program data stream")
	}
	return nil
}
