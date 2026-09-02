package proctree

import (
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

func admit(command Command) (Command, error) {
	if !validCommandBounds(command) || invalidStrings(command.Arguments, MaxArgumentBytes, false) ||
		invalidStrings(command.Environment, MaxEnvironmentBytes, true) {
		return Command{}, ErrInvalid
	}
	cleanup := command.CleanupTimeout
	if cleanup == 0 {
		cleanup = DefaultCleanupTimeout
	}
	return Command{
		Executable: command.Executable, Arguments: append([]string(nil), command.Arguments...),
		Directory: command.Directory, Environment: append([]string{}, command.Environment...),
		Input: append([]byte(nil), command.Input...), StdoutLimit: command.StdoutLimit, StderrLimit: command.StderrLimit,
		Timeout: command.Timeout, CleanupTimeout: cleanup,
	}, nil
}

func validCommandBounds(command Command) bool {
	return validCommandPaths(command) && validCommandCounts(command) && validCommandDurations(command)
}

func validCommandPaths(command Command) bool {
	return validAbsolutePath(command.Executable) && (command.Directory == "" || validAbsolutePath(command.Directory))
}

func validAbsolutePath(value string) bool {
	return len(value) <= MaxPathBytes && utf8.ValidString(value) && strings.IndexByte(value, 0) < 0 && filepath.IsAbs(value)
}

func validCommandCounts(command Command) bool {
	return len(command.Arguments) <= MaxArguments && len(command.Environment) <= MaxEnvironment && len(command.Input) <= MaxInputBytes &&
		command.StdoutLimit > 0 && command.StdoutLimit <= MaxOutputBytes && command.StderrLimit > 0 && command.StderrLimit <= MaxOutputBytes
}

func validCommandDurations(command Command) bool {
	return command.Timeout >= 0 && command.Timeout <= MaxTimeout && command.CleanupTimeout >= 0 && command.CleanupTimeout <= MaxCleanupTimeout
}

func invalidStrings(values []string, maximum int, environment bool) bool {
	total := 0
	var environmentNames [MaxEnvironment]string
	environmentCount := 0
	for _, value := range values {
		if len(value) > maximum-total || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return true
		}
		total += len(value)
		if environment {
			name, _, found := strings.Cut(value, "=")
			if !found || !validEnvironmentName(name) || duplicateEnvironmentName(environmentNames[:environmentCount], name, runtime.GOOS == "windows") {
				return true
			}
			environmentNames[environmentCount] = name
			environmentCount++
		}
	}
	return false
}

func validEnvironmentName(value string) bool {
	if value == "" || !environmentNameStart(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !environmentNamePart(value[index]) {
			return false
		}
	}
	return true
}

func environmentNameStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func environmentNamePart(value byte) bool {
	return environmentNameStart(value) || value >= '0' && value <= '9'
}

func duplicateEnvironmentName(previous []string, name string, foldCase bool) bool {
	for _, value := range previous {
		if value == name || foldCase && strings.EqualFold(value, name) {
			return true
		}
	}
	return false
}
