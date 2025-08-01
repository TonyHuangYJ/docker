package client

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/moby/moby/api/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/versions"
)

// ContainerExecCreate creates a new exec configuration to run an exec process.
func (cli *Client) ContainerExecCreate(ctx context.Context, containerID string, options container.ExecOptions) (container.ExecCreateResponse, error) {
	containerID, err := trimID("container", containerID)
	if err != nil {
		return container.ExecCreateResponse{}, err
	}

	// Make sure we negotiated (if the client is configured to do so),
	// as code below contains API-version specific handling of options.
	//
	// Normally, version-negotiation (if enabled) would not happen until
	// the API request is made.
	if err := cli.checkVersion(ctx); err != nil {
		return container.ExecCreateResponse{}, err
	}

	if err := cli.NewVersionError(ctx, "1.25", "env"); len(options.Env) != 0 && err != nil {
		return container.ExecCreateResponse{}, err
	}
	if versions.LessThan(cli.ClientVersion(), "1.42") {
		options.ConsoleSize = nil
	}

	resp, err := cli.post(ctx, "/containers/"+containerID+"/exec", nil, options, nil)
	defer ensureReaderClosed(resp)
	if err != nil {
		return container.ExecCreateResponse{}, err
	}

	var response container.ExecCreateResponse
	err = json.NewDecoder(resp.Body).Decode(&response)
	return response, err
}

// ContainerExecStart starts an exec process already created in the docker host.
func (cli *Client) ContainerExecStart(ctx context.Context, execID string, config container.ExecStartOptions) error {
	if versions.LessThan(cli.ClientVersion(), "1.42") {
		config.ConsoleSize = nil
	}
	resp, err := cli.post(ctx, "/exec/"+execID+"/start", nil, config, nil)
	ensureReaderClosed(resp)
	return err
}

// ContainerExecAttach attaches a connection to an exec process in the server.
//
// It returns a [types.HijackedResponse] with the hijacked connection
// and the a reader to get output. It's up to the called to close
// the hijacked connection by calling [types.HijackedResponse.Close].
//
// The stream format on the response uses one of two formats:
//
//   - If the container is using a TTY, there is only a single stream (stdout), and
//     data is copied directly from the container output stream, no extra
//     multiplexing or headers.
//   - If the container is *not* using a TTY, streams for stdout and stderr are
//     multiplexed.
//
// You can use [github.com/moby/moby/api/stdcopy.StdCopy] to demultiplex this
// stream. Refer to [Client.ContainerAttach] for details about the multiplexed
// stream.
func (cli *Client) ContainerExecAttach(ctx context.Context, execID string, config container.ExecAttachOptions) (types.HijackedResponse, error) {
	if versions.LessThan(cli.ClientVersion(), "1.42") {
		config.ConsoleSize = nil
	}
	return cli.postHijacked(ctx, "/exec/"+execID+"/start", nil, config, http.Header{
		"Content-Type": {"application/json"},
	})
}

// ContainerExecInspect returns information about a specific exec process on the docker host.
func (cli *Client) ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error) {
	var response container.ExecInspect
	resp, err := cli.get(ctx, "/exec/"+execID+"/json", nil, nil)
	if err != nil {
		return response, err
	}

	err = json.NewDecoder(resp.Body).Decode(&response)
	ensureReaderClosed(resp)
	return response, err
}
