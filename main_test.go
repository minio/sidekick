package main

import (
	"testing"
)

func TestGetHealthCheckURL(t *testing.T) {
	// ----- conditions -------------------------------------------------------
	testCases := []struct {
		name            string
		endpoint        string
		healthCheckPath string
		healthCheckPort int
		want            string
	}{
		{
			name:            "PortSetToZero",
			endpoint:        "http://minio1:9000",
			healthCheckPath: "/minio/health/ready",
			want:            "http://minio1:9000/minio/health/ready",
		},
		{
			name:            "PortSetToNonZeroValue",
			endpoint:        "http://minio1:9000",
			healthCheckPath: "/minio/health/ready",
			healthCheckPort: 4242,
			want:            "http://minio1:4242/minio/health/ready",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// ----- call -----------------------------------------------------
			healthCheckURL := getHealthCheckURL(tc.endpoint, tc.healthCheckPath, tc.healthCheckPort)

			// ----- verify ---------------------------------------------------
			if healthCheckURL != tc.want {
				t.Errorf("Expected %q, got %q", tc.want, healthCheckURL)
			}
		})
	}
}
