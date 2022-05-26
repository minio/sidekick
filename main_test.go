// Copyright (c) 2020 MinIO, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"testing"
)

func TestGetHealthCheckURL_Valid(t *testing.T) {
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
		{
			name:            "PortSetToUpperLimit",
			endpoint:        "http://minio1:9000",
			healthCheckPath: "/minio/health/ready",
			healthCheckPort: portUpperLimit,
			want:            "http://minio1:65535/minio/health/ready",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// ----- call -----------------------------------------------------
			healthCheckURL, err := getHealthCheckURL(tc.endpoint, tc.healthCheckPath, tc.healthCheckPort)
			// ----- verify ---------------------------------------------------
			if err != nil {
				t.Errorf("Expected no error, got %q", err)
			}

			if healthCheckURL != tc.want {
				t.Errorf("Expected %q, got %q", tc.want, healthCheckURL)
			}
		})
	}
}

func TestGetHealthCheckURL_Invalid(t *testing.T) {
	// ----- conditions -------------------------------------------------------
	want := ""

	testCases := []struct {
		name            string
		endpoint        string
		healthCheckPath string
		healthCheckPort int
	}{
		{
			name:     "BadEndpoint",
			endpoint: "bad",
		},
		{
			name:            "PortNumberBelowLowerLimit",
			endpoint:        "http://minio1:9000",
			healthCheckPath: "/minio/health/ready",
			healthCheckPort: portLowerLimit - 1,
		},
		{
			name:            "PortNumberAboveUpperLimit",
			endpoint:        "http://minio1:9000",
			healthCheckPath: "/minio/health/ready",
			healthCheckPort: portUpperLimit + 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// ----- call -----------------------------------------------------
			healthCheckURL, err := getHealthCheckURL(tc.endpoint, tc.healthCheckPath, tc.healthCheckPort)

			// ----- verify ---------------------------------------------------
			if err == nil {
				t.Errorf("Expected an error")
			}

			if healthCheckURL != want {
				t.Errorf("Expected %q, got %q", want, healthCheckURL)
			}
		})
	}
}
