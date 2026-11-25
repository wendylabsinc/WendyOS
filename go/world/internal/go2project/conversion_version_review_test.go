package go2project

import (
	"bytes"
	"os"
	"testing"
)

func TestConversionCompilerVersionsRemainExplicit(t *testing.T) {
	root, project, report, conversion := fixture(t)
	var previousDigest string
	for _, version := range []string{"3.3.7", "4.0.0"} {
		t.Run(version, func(t *testing.T) {
			conversion.Compiler.Version = version
			raw := encoded(t, conversion)
			write(t, report, raw)
			projection, err := ValidateConversion(root, project, report)
			if err != nil {
				t.Fatal(err)
			}
			if projection.ConversionSHA256 != hash(raw) || projection.ConversionSHA256 == previousDigest {
				t.Fatal("report identity did not preserve the explicit compiler version")
			}
			previousDigest = projection.ConversionSHA256
			after, err := os.ReadFile(report)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("reading a conversion changed its historical bytes")
			}
		})
	}
	for _, version := range []string{"", "4", "4.0", "4.0.1", "4.0.0-dev", "4.0.0 ", "3.3.6", "3.3.8", "5.0.0"} {
		t.Run("refuse_"+version, func(t *testing.T) {
			conversion.Compiler.Version = version
			write(t, report, encoded(t, conversion))
			if _, err := ValidateConversion(root, project, report); err == nil {
				t.Fatal("unreviewed compiler version accepted")
			}
		})
	}
}

func TestConversionCurrentRuntimeDoesNotRelaxAuthorityOrLimits(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*Conversion)
	}{
		{"operational", func(c *Conversion) { c.Operational = true }},
		{"physical", func(c *Conversion) { c.Domain = "physical" }},
		{"compiler", func(c *Conversion) { c.Compiler.Name = "Other" }},
		{"unbounded memory", func(c *Conversion) { c.Compiler.Limits["address_space_bytes"] = 0 }},
		{"longer deadline", func(c *Conversion) { c.Compiler.Limits["wall_seconds"] = 60 }},
		{"sandbox assertion", func(c *Conversion) { c.Compiler.Isolation = "security sandbox" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			root, project, report, conversion := fixture(t)
			conversion.Compiler.Version = "4.0.0"
			change.apply(&conversion)
			write(t, report, encoded(t, conversion))
			if _, err := ValidateConversion(root, project, report); err == nil {
				t.Fatal("current compiler version bypassed an existing limit")
			}
		})
	}
}
