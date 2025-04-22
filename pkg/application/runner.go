package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// Runner handles the application execution lifecycle
type Runner struct {
	cmd       []string
	immediate bool
	readyChan chan struct{}
	sigChan   chan os.Signal
	wg        *sync.WaitGroup
}

// NewRunner creates a new application runner
func NewRunner(cmd []string, startImmediately bool, readyChan chan struct{}, wg *sync.WaitGroup) *Runner {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	return &Runner{
		cmd:       cmd,
		immediate: startImmediately,
		readyChan: readyChan,
		sigChan:   sigChan,
		wg:        wg,
	}
}

// Execute runs the application with proper lifecycle management
func (r *Runner) Execute(ctx context.Context) (int, error) {
	// Handle the case where no command is provided
	if len(r.cmd) == 0 {
		return r.handleNoCommand(ctx)
	}

	// Find the command executable
	cmdPath, err := exec.LookPath(r.cmd[0])
	if err != nil {
		slog.Error("Failed to find command executable", "command", r.cmd[0], "error", err)
		return 127, fmt.Errorf("command not found: %w", err) // 127 is standard for command not found
	}

	// Wait for dependencies if needed
	if !r.immediate && r.readyChan != nil {
		slog.Info("Waiting for dependencies before starting command...")
		select {
		case <-r.readyChan:
			slog.Info("Dependencies ready. Starting command.")
		case <-ctx.Done():
			slog.Error("Timeout reached while waiting for dependencies.", "error", ctx.Err())
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 124, context.DeadlineExceeded // 124 is standard for timeout
			}
			return 1, ctx.Err()
		case sig := <-r.sigChan:
			slog.Info("Received signal while waiting for dependencies. Exiting.", "signal", sig)
			return 0, nil // Clean exit on signal
		}
	}

	// Start the child process
	exitCode, err := r.startProcess(ctx, cmdPath)
	return exitCode, err
}

// handleNoCommand manages the case where no command was specified
func (r *Runner) handleNoCommand(ctx context.Context) (int, error) {
	// Wait for dependencies to be ready or wait for signal
	if !r.immediate && r.readyChan != nil {
		slog.Info("No command specified. Waiting for dependencies to be ready...")
		select {
		case <-r.readyChan:
			slog.Info("Dependencies ready. Idling until terminated.")
		case <-ctx.Done():
			slog.Error("Timeout reached while waiting for dependencies.", "error", ctx.Err())
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 124, context.DeadlineExceeded // 124 is standard for timeout
			}
			return 1, ctx.Err()
		case sig := <-r.sigChan:
			slog.Info("Received signal while waiting for dependencies. Exiting.", "signal", sig)
			return 0, nil
		}
	} else {
		slog.Info("No command specified. Idling until terminated.")
	}

	// Wait for termination signal
	sig := <-r.sigChan
	slog.Info("Received termination signal. Exiting.", "signal", sig)
	return 0, nil
}

// startProcess starts and manages the child process
func (r *Runner) startProcess(ctx context.Context, cmdPath string) (int, error) {
	exitChan := make(chan error, 1)

	// Prepare command
	cmd := exec.Command(cmdPath, r.cmd[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Start the process
	slog.Info("Starting child command", "command", strings.Join(r.cmd, " "))
	if err := cmd.Start(); err != nil {
		slog.Error("Failed to start command", "command", cmdPath, "error", err)
		return 126, fmt.Errorf("failed to start command: %w", err) // 126 is standard for "not executable"
	}

	slog.Info("Child process started", "pid", cmd.Process.Pid)

	// Wait for command to finish in a goroutine
	go func() {
		exitChan <- cmd.Wait()
	}()

	// Handle signals and wait for command to complete
	return r.handleSignalsAndWait(cmd, exitChan)
}

// handleSignalsAndWait manages signal forwarding and waits for child process to exit
func (r *Runner) handleSignalsAndWait(cmd *exec.Cmd, exitChan chan error) (int, error) {
	exitCode := 0
	keepRunning := true

	for keepRunning {
		select {
		case sig := <-r.sigChan:
			slog.Info("Received signal", "signal", sig)
			if cmd.Process != nil {
				slog.Info("Forwarding signal to child process group", "signal", sig, "pgid", cmd.Process.Pid)

				// Try to signal the process group
				errSig := syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
				if errSig != nil {
					slog.Error("Failed to forward signal to child process group", "error", errSig)

					// Fall back to signaling just the process
					if errFallback := cmd.Process.Signal(sig); errFallback != nil {
						slog.Error("Failed to forward signal to child process", "error", errFallback)
					}
				}
			} else {
				slog.Warn("Received signal but child process is not running.")
				keepRunning = false
			}

		case err := <-exitChan:
			slog.Info("Child process exited.")
			if err != nil {
				if exitError, ok := err.(*exec.ExitError); ok {
					exitCode = exitError.ExitCode()
				} else {
					slog.Error("Error waiting for child process", "error", err)
					exitCode = 1
				}
			} else {
				exitCode = 0
			}
			keepRunning = false
		}
	}

	return exitCode, nil
}
