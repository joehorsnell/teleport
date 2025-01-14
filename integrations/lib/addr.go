// Copyright 2023 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lib

import (
	"net/url"
	"strings"

	"github.com/gravitational/trace"
)

// AddrToURL transforms an address string that may or may not contain
// a leading protocol or trailing port number into a well-formed URL
func AddrToURL(addr string) (*url.URL, error) {
	var (
		result *url.URL
		err    error
	)
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "https://" + addr
	}
	if result, err = url.Parse(addr); err != nil {
		return nil, trace.Wrap(err)
	}
	if result.Scheme == "https" && result.Port() == "443" {
		// Cut off redundant :443
		result.Host = result.Hostname()
	}
	return result, nil
}
