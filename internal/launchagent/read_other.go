//go:build !darwin

package launchagent

import "context"

func ReadInstalled(context.Context) (Spec, error) { return Spec{}, ErrUnsupported }
