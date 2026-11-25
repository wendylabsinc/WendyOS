package sessionbroker

import (
	"context"
	"os"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

// prepareOperatorDirectory is used only by requested device/mesh discovery.
// It uses the selected cached primary credential and a bounded LAN browse;
// discovering nearby devices never starts a Cloud login or credential renewal.
func prepareOperatorDirectory(ctx context.Context) (*grpcclient.AgentConnection, error) {
	certificate, operator, err := configuredOperator()
	if err != nil {
		return nil, err
	}
	directory, err := configDir()
	if err != nil {
		return nil, err
	}
	recipe, err := readOperatorRecipe(operatorRecipePath(directory, operator.Principal()))
	if os.IsNotExist(err) {
		return PrepareOperatorAutomatic(ctx, certificate)
	}
	if err != nil {
		return nil, err
	}
	spec, err := operatorSpec(recipe, certificate)
	if err != nil {
		return nil, err
	}
	return connectOrStartOperator(ctx, spec, &certificate)
}

// ReadOperatorDirectoryAutomatic may prepare a cold context as part of an
// explicitly requested Nearby scan. ReadOperatorDirectory remains available
// to callers that require a strictly observational, already-connected read.
func ReadOperatorDirectoryAutomatic(ctx context.Context) (OperatorDirectory, error) {
	owner, err := prepareOperatorDirectory(ctx)
	if err != nil {
		return OperatorDirectory{Version: 1, State: "not-connected", Devices: []OperatorTargetInfo{}}, err
	}
	defer owner.Close()
	return ReadOperatorDirectory(ctx)
}

func WatchOperatorDirectoryAutomatic(ctx context.Context) (<-chan OperatorDirectory, error) {
	owner, err := prepareOperatorDirectory(ctx)
	if err != nil {
		return nil, err
	}
	defer owner.Close()
	return WatchOperatorDirectory(ctx)
}
