package ghcli

import "strings"

func isProjectScopeError(err error) bool {
	if err == nil {
		return false
	}
	return isProjectScopeErrorText(err.Error())
}

func isProjectScopeErrorText(msg string) bool {
	msg = strings.ToLower(msg)
	if strings.Contains(msg, "insufficient_scopes") {
		return true
	}
	if strings.Contains(msg, "read:project") {
		return true
	}
	if strings.Contains(msg, "required scopes") && strings.Contains(msg, "project") {
		return true
	}
	if strings.Contains(msg, "missing 'project' scope") {
		return true
	}
	return false
}
