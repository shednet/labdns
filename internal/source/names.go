/*
Copyright 2026 Konstantinos Kalyvas.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package source

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

func NormalizeHostname(value string) (string, error) {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	check := value
	if remainder, wildcard := strings.CutPrefix(value, "*."); wildcard {
		check = remainder
		if strings.Contains(check, "*") {
			return "", Invalid(fmt.Errorf("wildcard is permitted only as the complete first label"))
		}
	} else if strings.Contains(value, "*") {
		return "", Invalid(fmt.Errorf("wildcard is permitted only as the complete first label"))
	}
	if errs := validation.IsDNS1123Subdomain(check); len(errs) != 0 {
		return "", Invalid(fmt.Errorf("not a DNS hostname: %s", strings.Join(errs, ", ")))
	}
	return value, nil
}

// matchingZone returns the longest label-boundary suffix, or an empty string.
func matchingZone(hostname string, zones []string) string {
	hostname = strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(hostname, ".")), "*.")
	best := ""
	for _, zone := range zones {
		zone = strings.ToLower(strings.TrimSuffix(zone, "."))
		if (hostname == zone || strings.HasSuffix(hostname, "."+zone)) && len(zone) > len(best) {
			best = zone
		}
	}
	return best
}
