/*
Copyright © 2025 AJ Roetker ajroetker@antfly.io

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

package main

import (
	"io"
	"log"
	"os"
	"runtime"
	"runtime/debug"

	"github.com/antflydb/antfly/cmd/antfly/cmd"
	"github.com/antflydb/antfly/lib/utils"

	json "github.com/antflydb/antfly/pkg/libaf/json"

	gojson "github.com/goccy/go-json"
)

const (
	enableMutexProfiling = false
	enableBlockProfiling = false
)

var (
	// Set by GoReleaser via ldflags.
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func init() {
	configureJSON()
	configureRuntime()
	configureVersionMetadata()
}

func main() {
	if err := execute(); err != nil {
		log.Printf("Fatal error: %v", err)
		os.Exit(1)
	}
}

func execute() error {
	cmd.Execute()
	return nil
}

func configureJSON() {
	json.SetConfig(json.Config{
		Marshal: gojson.Marshal,

		MarshalIndent: func(
			v any,
			prefix string,
			indent string,
		) ([]byte, error) {
			return gojson.MarshalIndent(
				v,
				prefix,
				indent,
			)
		},

		Unmarshal: gojson.Unmarshal,

		MarshalString: marshalString,

		UnmarshalString: func(
			s string,
			v any,
		) error {
			return gojson.Unmarshal([]byte(s), v)
		},

		NewEncoder: func(w io.Writer) json.Encoder {
			encoder := gojson.NewEncoder(w)

			encoder.SetEscapeHTML(false)

			return encoder
		},

		NewDecoder: func(r io.Reader) json.Decoder {
			return gojson.NewDecoder(r)
		},
	})
}

func marshalString(v any) (string, error) {
	data, err := gojson.Marshal(v)
	if err != nil {
		return "", err
	}

	return bytesToString(data), nil
}

func configureRuntime() {
	// Improve GC behavior for large workloads.
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Return memory to OS more aggressively.
	debug.SetGCPercent(100)

	if enableMutexProfiling {
		runtime.SetMutexProfileFraction(1)
	}

	if enableBlockProfiling {
		runtime.SetBlockProfileRate(1)
	}
}

func configureVersionMetadata() {
	cmd.Version = version
	utils.Version = version

	log.Printf(
		"Starting Antfly version=%s commit=%s date=%s",
		version,
		commit,
		date,
	)
}

// bytesToString avoids an unnecessary allocation.
// Use carefully: the byte slice must not be modified after conversion.
func bytesToString(b []byte) string {
	return unsafeString(b)
}
