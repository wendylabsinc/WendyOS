package appconfig

import (
	"strings"
	"testing"
)

func TestCheckDeploymentConfig(t *testing.T) {
	for config, ok := range map[string]bool{
		`{}`:                   true,
		` {"A":"1"} `:          true,
		`{"WENDY_DEVICE":"x"}`: false, // reserved: the agent's own
		`{"LD_PRELOAD":"x"}`:   false,
		``:                     false,
		`null`:                 false,
		`[]`:                   false,
		`"x"`:                  false,
		`{} {}`:                false,
		`{"a":1`:               false,
		`{"a":"` + strings.Repeat("x", MaxDeploymentConfigSize) + `"}`: false,
		`{"a":{"a":1},"b":[{"a":1},{"a":2}]}`:                          false, // named values only
		`{"A":"x","B":1.5,"C":true}`:                                   true,
		`{"A":null}`:                                                   false,
		`{"bad-name":"x"}`:                                             false,
		`{"a":1,"a":2}`:                                                false,
		`{"b":{"a":1,"a":2}}`:                                          false,
		`{"c":[{"a":1,"a":2}]}`:                                        false,
		`{"a":}`:                                                       false,
		`{"env":1,"ENV":2}`:                                            false,
		`{"k":1,"\u212a":2}`:                                           false, // Kelvin sign folds to k
		`{"a":{"env":1},"b":{"ENV":1}}`:                                false, // nested: not a named value
		`{"a":1e400}`:                                                  true,
		`{"a":123456789012345678901234567890}`:                         true,
		// As strict as the runtime: the agent's own variables, a linker's,
		// and values the runtime would refuse.
		`{"ROS_DOMAIN_ID":"7"}`:                          false,
		`{"otel_service_name":"x"}`:                      false,
		`{"RMW_IMPLEMENTATION":"x"}`:                     false,
		`{"CYCLONEDDS_URI":"x"}`:                         false,
		`{"ld_preload":"x"}`:                             false,
		`{"PEM":"a\nb"}`:                                 false,
		`{"BIG":"` + strings.Repeat("x", 33*1024) + `"}`: false,
		`{"ROSETTA":"ok","OTELX":"ok"}`:                  true,
	} {
		if err := CheckDeploymentConfig(config); (err == nil) != ok {
			t.Errorf("CheckDeploymentConfig(%.30q) = %v, want ok %v", config, err, ok)
		}
	}
}
