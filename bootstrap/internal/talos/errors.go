package talos

import "strings"

// ErrorCode represents different types of Talos errors
type ErrorCode int

const (
	ErrUnknown ErrorCode = iota
	ErrAlreadyConfigured
	ErrCertificateRequired
	ErrConnectionRefused
	ErrMaintenanceMode
	ErrAlreadyBootstrapped
	ErrConnectionTimeout
	ErrNodeNotReady
	ErrPermissionDenied
)

// TalosError represents a classified Talos API error
type TalosError struct {
	Code      ErrorCode
	Message   string
	Wrapped   error
	Retryable bool
}

func (e *TalosError) Error() string {
	if e.Wrapped != nil {
		return e.Message + ": " + e.Wrapped.Error()
	}
	return e.Message
}

func (e *TalosError) Unwrap() error {
	return e.Wrapped
}

// ParseTalosError analyzes an error and classifies it for retry logic
func ParseTalosError(err error) *TalosError {
	if err == nil {
		return nil
	}

	errStr := strings.ToLower(err.Error())

	// Already configured - not an error if node is ready
	if strings.Contains(errStr, "already configured") ||
		strings.Contains(errStr, "configuration already applied") {
		return &TalosError{
			Code:      ErrAlreadyConfigured,
			Message:   "node already configured",
			Wrapped:   err,
			Retryable: false,
		}
	}

	// Certificate required - switch to secure mode
	if strings.Contains(errStr, "certificate required") ||
		strings.Contains(errStr, "tls handshake") ||
		strings.Contains(errStr, "certificate is required") ||
		strings.Contains(errStr, "certificate signed by unknown authority") {
		return &TalosError{
			Code:      ErrCertificateRequired,
			Message:   "certificate required for secure connection",
			Wrapped:   err,
			Retryable: true,
		}
	}

	// Connection refused - node might be rebooting
	if strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connect: connection refused") {
		return &TalosError{
			Code:      ErrConnectionRefused,
			Message:   "connection refused - node may be rebooting",
			Wrapped:   err,
			Retryable: true,
		}
	}

	// Connection timeout - network issue or node down
	if strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "context deadline exceeded") ||
		strings.Contains(errStr, "i/o timeout") {
		return &TalosError{
			Code:      ErrConnectionTimeout,
			Message:   "connection timeout",
			Wrapped:   err,
			Retryable: true,
		}
	}

	// Node not ready - still booting (check this BEFORE maintenance mode)
	if strings.Contains(errStr, "node not ready") ||
		strings.Contains(errStr, "service not running") {
		return &TalosError{
			Code:      ErrNodeNotReady,
			Message:   "node not ready",
			Wrapped:   err,
			Retryable: true,
		}
	}

	// Maintenance mode - node is in maintenance (broader check after specific ones)
	// Only match standalone "not ready" here, not "node not ready"
	if strings.Contains(errStr, "maintenance") ||
		(strings.Contains(errStr, "not ready") && !strings.Contains(errStr, "node not ready")) {
		return &TalosError{
			Code:      ErrMaintenanceMode,
			Message:   "node is in maintenance mode",
			Wrapped:   err,
			Retryable: true,
		}
	}

	// Already bootstrapped - not an error
	if strings.Contains(errStr, "already bootstrapped") ||
		strings.Contains(errStr, "etcd already initialized") ||
		strings.Contains(errStr, "etcd data directory is not empty") ||
		(strings.Contains(errStr, "alreadyexists") && strings.Contains(errStr, "etcd")) {
		return &TalosError{
			Code:      ErrAlreadyBootstrapped,
			Message:   "cluster already bootstrapped",
			Wrapped:   err,
			Retryable: false,
		}
	}

	// Permission denied - likely a credentials issue or node misconfiguration
	if strings.Contains(errStr, "permission denied") ||
		strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "forbidden") {
		return &TalosError{
			Code:      ErrPermissionDenied,
			Message:   "permission denied",
			Wrapped:   err,
			Retryable: false,
		}
	}

	// Unknown error - retry with caution
	return &TalosError{
		Code:      ErrUnknown,
		Message:   "unknown error",
		Wrapped:   err,
		Retryable: false,
	}
}

// IsRetryable checks if the error is classified as retryable
func (e *TalosError) IsRetryable() bool {
	return e.Retryable
}

// ShouldSwitchToSecure checks if the error indicates a certificate issue that requires switching to secure mode
func (e *TalosError) ShouldSwitchToSecure() bool {
	return e.Code == ErrCertificateRequired
}

// IsSuccessState returns true if the error indicates a success state
func (e *TalosError) IsSuccessState() bool {
	return e.Code == ErrAlreadyConfigured || e.Code == ErrAlreadyBootstrapped
}
