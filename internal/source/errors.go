/*
Copyright 2026 Konstantinos Kalyvas.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package source

import "errors"

// invalidError marks state that cannot become valid without changing a watched
// source or dependency. Controllers use it to avoid deterministic retries.
type invalidError struct {
	Err error
}

type dependencyError struct {
	reason string
	err    error
}

func (e dependencyError) Error() string         { return e.err.Error() }
func (e dependencyError) Unwrap() error         { return e.err }
func (e dependencyError) WarningReason() string { return e.reason }

func dependency(reason string, err error) error {
	return dependencyError{reason: reason, err: err}
}

func (e invalidError) Error() string { return e.Err.Error() }

func (e invalidError) Unwrap() error { return e.Err }

// Invalid marks err as a deterministic source/configuration error.
func Invalid(err error) error {
	if err == nil {
		return nil
	}
	if IsInvalid(err) {
		return err
	}
	return invalidError{Err: err}
}

// IsInvalid reports whether err contains a deterministic source/configuration
// error, even when callers add context around it.
func IsInvalid(err error) bool {
	var target invalidError
	return errors.As(err, &target)
}
