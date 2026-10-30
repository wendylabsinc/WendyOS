package commands

import (
	"errors"
	"fmt"
	"os"
)

const cloudUploadRetryHint = "Check that the device is online with 'wendy cloud discover', then retry."
const cloudRegistryRetryHint = "Use '--chunking=off' to upload through the registry with the usual registry authorization."
const cloudRegistryFallbackNote = "Automatic registry fallback is off after this failure."

// Keep transport diagnostics inspectable without putting raw server text in
// the user-facing message.
type cloudDeployError struct {
	message string
	cause   error
}

func (e *cloudDeployError) Error() string { return e.message }
func (e *cloudDeployError) Unwrap() error { return e.cause }

// Preparation can fail while chunks are still being sent. Mark its origin so
// an Unavailable preparation response is not mistaken for an upload drop.
type cloudImagePrepareError struct{ error }

func (e *cloudImagePrepareError) Unwrap() error { return e.error }

func cloudChunkTransferError(host string, snap chunkPushSnapshot, uploadErr, reconnectErr error) error {
	message := fmt.Sprintf("The Cloud connection to %s was lost during the upload. Sent %s of %s planned.", host, formatBytes(snap.SentBytes), formatBytes(snap.PlannedBytes))
	if reconnectErr != nil {
		message += " Could not reconnect."
	}
	message += "\n" + cloudRegistryFallbackNote
	cause := errors.Join(uploadErr, reconnectErr)
	if os.Getenv("WENDY_TLS_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[chunk-upload] %v\n", cause)
	}
	return withNextSteps(classifyCommandError(errDeviceUnreachable, &cloudDeployError{message: message, cause: cause}), cloudUploadRetryHint, cloudRegistryRetryHint)
}

func cloudRunStartConnectionError(host string, cause error) error {
	if os.Getenv("WENDY_TLS_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[chunk-deploy] %v\n", cause)
	}
	message := fmt.Sprintf("The Cloud connection to %s was lost while starting the app.\n%s", host, cloudRegistryFallbackNote)
	return withNextSteps(classifyCommandError(errDeviceUnreachable, &cloudDeployError{message: message, cause: cause}), cloudUploadRetryHint, cloudRegistryRetryHint)
}

func cloudChunkDeployFailure(err error) error {
	if isImageBuildFailure(err) {
		return err
	}
	if isUnimplementedRPCError(err) {
		return withNextSteps(&cloudDeployError{message: "This Agent does not support this upload method.", cause: err}, "Update the Agent, then retry.")
	}
	var preparation *cloudImagePrepareError
	if errors.As(err, &preparation) {
		return withNextSteps(&cloudDeployError{message: "The device could not prepare the app image.", cause: err}, "Check the device's logs, then retry.")
	}
	return err
}
