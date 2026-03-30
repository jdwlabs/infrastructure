package talos

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestErrorCodeConstants verifies error codes are sequential and unique
func TestErrorCodeConstants(t *testing.T) {
	tests := []struct {
		code     ErrorCode
		expected int
		name     string
	}{
		{ErrUnknown, 0, "ErrUnknown"},
		{ErrAlreadyConfigured, 1, "ErrAlreadyConfigured"},
		{ErrCertificateRequired, 2, "ErrCertificateRequired"},
		{ErrConnectionRefused, 3, "ErrConnectionRefused"},
		{ErrMaintenanceMode, 4, "ErrMaintenanceMode"},
		{ErrAlreadyBootstrapped, 5, "ErrAlreadyBootstrapped"},
		{ErrConnectionTimeout, 6, "ErrConnectionTimeout"},
		{ErrNodeNotReady, 7, "ErrNodeNotReady"},
		{ErrPermissionDenied, 8, "ErrPermissionDenied"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, ErrorCode(tt.expected), tt.code, "Error code %s should have value %d", tt.name, tt.expected)
		})
	}
}

// TestParseTalosError_NilInput verifies nil handling
func TestParseTalosError_NilInput(t *testing.T) {
	result := ParseTalosError(nil)
	assert.Nil(t, result, "ParseTalosError(nil) should return nil")
}

// TestParseTalosError_TableDriven comprehensive error classification tests
func TestParseTalosError_TableDriven(t *testing.T) {
	type testCase struct {
		name           string
		input          error
		expectedCode   ErrorCode
		expectedMsg    string
		isRetryable    bool
		switchToSecure bool
		isSuccess      bool
	}

	cases := []testCase{
		// Success states
		{
			name:         "already configured - exact match",
			input:        errors.New("already configured"),
			expectedCode: ErrAlreadyConfigured,
			expectedMsg:  "node already configured",
			isRetryable:  false,
			isSuccess:    true,
		},
		{
			name:         "configuration already applied",
			input:        errors.New("configuration already applied"),
			expectedCode: ErrAlreadyConfigured,
			isSuccess:    true,
		},
		{
			name:         "etcd already bootstrapped",
			input:        errors.New("etcd already bootstrapped"),
			expectedCode: ErrAlreadyBootstrapped,
			expectedMsg:  "cluster already bootstrapped",
			isSuccess:    true,
		},
		{
			name:         "etcd already initialized",
			input:        errors.New("etcd already initialized"),
			expectedCode: ErrAlreadyBootstrapped,
			isSuccess:    true,
		},
		{
			name:         "etcd data directory not empty",
			input:        errors.New("rpc error: code = AlreadyExists desc = etcd data directory is not empty"),
			expectedCode: ErrAlreadyBootstrapped,
			isSuccess:    true,
		},

		// Certificate errors (switch to secure)
		{
			name:           "certificate required",
			input:          errors.New("certificate is required"),
			expectedCode:   ErrCertificateRequired,
			expectedMsg:    "certificate required for secure connection",
			isRetryable:    true,
			switchToSecure: true,
		},
		{
			name:           "TLS handshake failed",
			input:          errors.New("tls handshake failed"),
			expectedCode:   ErrCertificateRequired,
			isRetryable:    true,
			switchToSecure: true,
		},
		{
			name:           "unknown certificate authority",
			input:          errors.New("certificate signed by unknown authority"),
			expectedCode:   ErrCertificateRequired,
			isRetryable:    true,
			switchToSecure: true,
		},

		// Connection errors (retryable)
		{
			name:         "connection refused",
			input:        errors.New("connection refused"),
			expectedCode: ErrConnectionRefused,
			expectedMsg:  "connection refused - node may be rebooting",
			isRetryable:  true,
		},
		{
			name:         "connect connection refused",
			input:        errors.New("connect: connection refused"),
			expectedCode: ErrConnectionRefused,
			isRetryable:  true,
		},
		{
			name:         "timeout",
			input:        errors.New("operation timeout"),
			expectedCode: ErrConnectionTimeout,
			expectedMsg:  "connection timeout",
			isRetryable:  true,
		},
		{
			name:         "context deadline exceeded",
			input:        errors.New("context deadline exceeded"),
			expectedCode: ErrConnectionTimeout,
			isRetryable:  true,
		},
		{
			name:         "I/O timeout",
			input:        errors.New("i/o timeout"),
			expectedCode: ErrConnectionTimeout,
			isRetryable:  true,
		},

		// Maintenance mode
		{
			name:         "maintenance mode",
			input:        errors.New("node is in maintenance mode"),
			expectedCode: ErrMaintenanceMode,
			isRetryable:  true,
		},
		{
			name:         "not ready",
			input:        errors.New("not ready"),
			expectedCode: ErrMaintenanceMode,
			isRetryable:  true,
		},

		// Permission errors (not retryable)
		{
			name:         "permission denied",
			input:        errors.New("permission denied"),
			expectedCode: ErrPermissionDenied,
			expectedMsg:  "permission denied",
			isRetryable:  false,
		},
		{
			name:         "unauthorized",
			input:        errors.New("unauthorized"),
			expectedCode: ErrPermissionDenied,
			isRetryable:  false,
		},
		{
			name:         "forbidden",
			input:        errors.New("forbidden"),
			expectedCode: ErrPermissionDenied,
			isRetryable:  false,
		},
		{
			name:         "unauthorized access",
			input:        errors.New("unauthorized access"),
			expectedCode: ErrPermissionDenied,
			isRetryable:  false,
		},

		// Node not ready
		{
			name:         "node not ready",
			input:        errors.New("node not ready"),
			expectedCode: ErrNodeNotReady,
			isRetryable:  true,
		},
		{
			name:         "service not running",
			input:        errors.New("service not running"),
			expectedCode: ErrNodeNotReady,
			isRetryable:  true,
		},

		// Unknown errors
		{
			name:         "unknown error",
			input:        errors.New("some random error"),
			expectedCode: ErrUnknown,
			expectedMsg:  "unknown error",
			isRetryable:  false,
		},
		{
			name:         "empty error",
			input:        errors.New(""),
			expectedCode: ErrUnknown,
			isRetryable:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := ParseTalosError(tc.input)
			require.NotNil(t, result, "Result should not be nil for non-nil input")

			assert.Equal(t, tc.expectedCode, result.Code, "Error code mismatch")
			assert.Equal(t, tc.isRetryable, result.IsRetryable(), "Retryable mismatch")
			assert.Equal(t, tc.switchToSecure, result.ShouldSwitchToSecure(), "SwitchToSecure mismatch")
			assert.Equal(t, tc.isSuccess, result.IsSuccessState(), "SuccessState mismatch")

			if tc.expectedMsg != "" {
				assert.Equal(t, tc.expectedMsg, result.Message, "Message mismatch")
			}

			// Verify wrapped error is preserved
			assert.Equal(t, tc.input, result.Wrapped, "Wrapped error should be preserved")
		})
	}
}

// TestParseTalosError_CaseVariations tests case insensitivity
func TestParseTalosError_CaseVariations(t *testing.T) {
	tests := []struct {
		name         string
		errStr       string
		expectedCode ErrorCode
	}{
		{
			name:         "uppercase already configured",
			errStr:       "ALREADY CONFIGURED",
			expectedCode: ErrAlreadyConfigured,
		},
		{
			name:         "mixed case TLS error",
			errStr:       "TLS Handshake Failed",
			expectedCode: ErrCertificateRequired,
		},
		{
			name:         "mixed case maintenance",
			errStr:       "Node Is In Maintenance Mode",
			expectedCode: ErrMaintenanceMode,
		},
		{
			name:         "lowercase connection refused",
			errStr:       "connect: connection refused",
			expectedCode: ErrConnectionRefused,
		},
		{
			name:         "timeout with context",
			errStr:       "Context Deadline Exceeded",
			expectedCode: ErrConnectionTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := errors.New(tt.errStr)
			talosErr := ParseTalosError(err)
			require.NotNil(t, talosErr)
			assert.Equal(t, tt.expectedCode, talosErr.Code)
		})
	}
}

// TestTalosError_ErrorMethod tests Error() string method
func TestTalosError_ErrorMethod(t *testing.T) {
	t.Run("Error with wrapped error", func(t *testing.T) {
		wrapped := errors.New("original error")
		talosErr := &TalosError{
			Code:    ErrUnknown,
			Message: "wrapper message",
			Wrapped: wrapped,
		}
		errStr := talosErr.Error()
		assert.Contains(t, errStr, "wrapper message")
		assert.Contains(t, errStr, "original error")
	})

	t.Run("Error without wrapped error", func(t *testing.T) {
		talosErr := &TalosError{
			Code:    ErrUnknown,
			Message: "simple message",
		}
		assert.Equal(t, "simple message", talosErr.Error())
	})
}

// TestTalosError_Unwrap tests error unwrapping
func TestTalosError_Unwrap(t *testing.T) {
	t.Run("Unwrap returns wrapped error", func(t *testing.T) {
		wrapped := errors.New("original")
		talosErr := &TalosError{Wrapped: wrapped}
		assert.Equal(t, wrapped, talosErr.Unwrap())
	})

	t.Run("Unwrap returns nil", func(t *testing.T) {
		talosErr := &TalosError{Message: "no wrap"}
		assert.Nil(t, talosErr.Unwrap())
	})
}

// TestTalosError_IsSuccessState tests success state detection
func TestTalosError_IsSuccessState(t *testing.T) {
	t.Run("IsSuccessState true cases", func(t *testing.T) {
		successCases := []ErrorCode{
			ErrAlreadyConfigured,
			ErrAlreadyBootstrapped,
		}
		for _, code := range successCases {
			err := &TalosError{Code: code}
			assert.True(t, err.IsSuccessState(), "Code %d should be success state", code)
		}
	})

	t.Run("IsSuccessState false cases", func(t *testing.T) {
		nonSuccessCases := []ErrorCode{
			ErrUnknown,
			ErrCertificateRequired,
			ErrConnectionRefused,
			ErrMaintenanceMode,
			ErrConnectionTimeout,
			ErrNodeNotReady,
			ErrPermissionDenied,
		}
		for _, code := range nonSuccessCases {
			err := &TalosError{Code: code}
			assert.False(t, err.IsSuccessState(), "Code %d should not be success state", code)
		}
	})
}

// TestTalosError_ImplementsError verifies interface compliance
func TestTalosError_ImplementsError(t *testing.T) {
	var _ error = &TalosError{}
}

// TestParseTalosError_Concurrent verifies thread safety
func TestParseTalosError_Concurrent(t *testing.T) {
	errs := []error{
		errors.New("already configured"),
		errors.New("connection refused"),
		errors.New("certificate required"),
		errors.New("timeout"),
		errors.New("permission denied"),
	}

	var wg sync.WaitGroup
	errChan := make(chan *TalosError, 500)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j, err := range errs {
				result := ParseTalosError(err)
				if result == nil {
					t.Errorf("Got nil result for error %d in goroutine %d", j, idx)
					continue
				}
				errChan <- result
			}
		}(i)
	}

	wg.Wait()
	close(errChan)

	count := 0
	for result := range errChan {
		assert.NotNil(t, result)
		assert.NotEqual(t, ErrUnknown, result.Code, "Should not get unknown for known errors")
		count++
	}
	assert.Equal(t, 500, count, "Expected 500 total results")
}

// TestTalosError_ErrorChaining tests errors.Is compatibility
func TestTalosError_ErrorChaining(t *testing.T) {
	rootCause := errors.New("root cause")
	wrapped := &TalosError{
		Code:    ErrConnectionRefused,
		Message: "connection failed",
		Wrapped: rootCause,
	}

	assert.True(t, errors.Is(wrapped, rootCause), "Should be able to detect root cause")
	assert.False(t, errors.Is(wrapped, errors.New("other")), "Should not match unrelated errors")
}

// Benchmarks

func BenchmarkParseTalosError(b *testing.B) {
	err := errors.New("connection refused: node may be rebooting")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ParseTalosError(err)
	}
}

func BenchmarkParseTalosError_Unknown(b *testing.B) {
	err := errors.New("some random unknown error message")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ParseTalosError(err)
	}
}

func BenchmarkTalosError_IsRetryable(b *testing.B) {
	err := &TalosError{Code: ErrConnectionRefused, Retryable: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = err.IsRetryable()
	}
}
