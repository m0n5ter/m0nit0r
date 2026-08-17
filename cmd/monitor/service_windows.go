package main

import (
	"context"

	"golang.org/x/sys/windows/svc"
)

// serviceName must match the name the service was registered under.
const serviceName = "m0nit0r"

// inServiceMode reports whether the process was started by the Windows Service
// Control Manager rather than from a console.
func inServiceMode() bool {
	isService, err := svc.IsWindowsService()
	return err == nil && isService
}

// runService hands control to the SCM, translating a stop request into
// cancellation of the application context.
func runService(run func(context.Context) error) error {
	handler := &serviceHandler{run: run}
	if err := svc.Run(serviceName, handler); err != nil {
		return err
	}
	return handler.err
}

type serviceHandler struct {
	run func(context.Context) error
	err error
}

func (h *serviceHandler) Execute(args []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()

	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case err := <-done:
			// The application stopped on its own, which for a service is
			// always a failure worth reporting to the SCM.
			h.err = err
			status <- svc.Status{State: svc.StopPending}
			if err != nil {
				return false, 1
			}
			return false, 0

		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- request.CurrentStatus

			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				h.err = <-done
				return false, 0
			}
		}
	}
}
