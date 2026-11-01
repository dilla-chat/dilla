package desktoplaunch

import (
	"encoding/json"
	"errors"
)

type Journal struct {
	Format            int    `json:"format"`
	Nonce             string `json:"nonce"`
	Phase             string `json:"phase"`
	ChildPID          *int64 `json:"childPid,omitempty"`
	WatchdogPID       *int64 `json:"watchdogPid,omitempty"`
	InstallerActorPID *int64 `json:"installerActorPid,omitempty"`
	InstallerPID      *int64 `json:"installerPid,omitempty"`
	Failure           string `json:"failure,omitempty"`
	Version           string `json:"version,omitempty"`
}

func ParseActiveRecord(raw []byte) (string, error) {
	if len(raw) > 256 {
		return "", errors.New("active pointer exceeds bound")
	}
	if e := validateJSON(raw); e != nil {
		return "", e
	}
	if _, e := exactObject(raw, "format", "nonce"); e != nil {
		return "", e
	}
	var a struct {
		Format int    `json:"format"`
		Nonce  string `json:"nonce"`
	}
	if e := json.Unmarshal(raw, &a); e != nil {
		return "", e
	}
	if a.Format != 1 || !noncePattern.MatchString(a.Nonce) {
		return "", errors.New("invalid active update pointer")
	}
	return a.Nonce, nil
}
func ParseJournal(raw []byte, nonce string) (*Journal, error) {
	if len(raw) > 1024 {
		return nil, errors.New("update journal exceeds bound")
	}
	if e := validateJSON(raw); e != nil {
		return nil, e
	}
	optional := []string{"childPid", "watchdogPid", "installerActorPid", "installerPid", "failure", "version"}
	obj, e := knownObject(raw, []string{"format", "nonce", "phase"}, optional)
	if e != nil {
		return nil, e
	}
	allowed := map[string]bool{"format": true, "nonce": true, "phase": true}
	for _, k := range optional {
		allowed[k] = true
	}
	for k := range obj {
		if !allowed[k] {
			return nil, errors.New("unknown update journal field")
		}
	}
	var j Journal
	if e = json.Unmarshal(raw, &j); e != nil {
		return nil, e
	}
	if j.Format != 1 || j.Nonce != nonce || !noncePattern.MatchString(nonce) {
		return nil, errors.New("invalid update journal identity")
	}
	switch j.Phase {
	case "prepared", "installing", "launching", "recovering", "failed", "complete":
	default:
		return nil, errors.New("invalid update journal phase")
	}
	for _, pid := range []*int64{j.ChildPID, j.WatchdogPID, j.InstallerActorPID, j.InstallerPID} {
		if pid != nil && (*pid <= 1 || *pid > 0xffffffff) {
			return nil, errors.New("invalid update journal PID")
		}
	}
	if j.Phase == "complete" {
		parts, e := versionParts(j.Version)
		if e != nil || parts[0] == 0 && parts[1] == 0 && parts[2] == 0 || j.ChildPID == nil {
			return nil, errors.New("invalid complete update journal")
		}
	} else if _, ok := obj["version"]; ok {
		return nil, errors.New("version on noncomplete update journal")
	}
	_, failurePresent := obj["failure"]
	if failurePresent && j.Failure != "installer" && j.Failure != "startup" && j.Failure != "interrupted" && j.Failure != "metadata" {
		return nil, errors.New("invalid update journal failure")
	}
	return &j, nil
}
