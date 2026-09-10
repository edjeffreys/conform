package run

import (
	"context"
	"fmt"
	"strings"
)

// ffmpeg's device-setup failures. A drifting list of strings is a poor primary
// defence, which is why the pre-flight exists; this catches what it cannot
// anticipate — a device that disappears between the check and the encode.
var deviceFaults = []string{
	"Error creating a MFX session",
	"Device creation failed",
	"No device available for decoder",
	"for option 'init_hw_device'",
	"Failed to initialise VAAPI connection",
	"No VA display found",
	"Cannot load libcuda",
}

// deviceFault reports the signature that says ffmpeg failed to set up its
// hardware, meaning the worker is broken rather than the file.
func deviceFault(stderr string) string {
	for _, sig := range deviceFaults {
		if strings.Contains(stderr, sig) {
			return sig
		}
	}
	return ""
}

// Returned rather than recorded: an excuse keys on the file's size and mtime,
// so charging a worker's fault to the file would excuse it permanently for a
// condition that has nothing to do with it. Non-zero instead lets a Job's
// backoffLimit retry it and then fail visibly.
func faultError(detail, stderr string) error {
	return fmt.Errorf("this worker cannot encode: %s\n%s", detail, stderr)
}

// checkDevice runs the cheapest thing that creates the device and nothing
// else, so a failure can only mean the device. Once per device per process:
// the answer cannot change under a running worker without the pod being
// replaced, and the encode that follows would fail on it anyway.
func (r *Runner) checkDevice(ctx context.Context, spec string) error {
	r.deviceMu.Lock()
	defer r.deviceMu.Unlock()
	if err, done := r.devices[spec]; done {
		return err
	}

	args := []string{"-hide_banner", "-nostdin", "-init_hw_device", spec,
		"-f", "lavfi", "-i", "nullsrc=s=64x64:d=0.1", "-f", "null", "-"}
	out, err := runFFmpeg(ctx, r.Exec.FFmpeg, args, nil)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		err = faultError(fmt.Sprintf("cannot open the hardware device %q the profile's encoder needs: %v", spec, err), out)
	} else {
		r.logf("hardware device %s opens", spec)
	}

	if r.devices == nil {
		r.devices = map[string]error{}
	}
	r.devices[spec] = err
	return err
}
