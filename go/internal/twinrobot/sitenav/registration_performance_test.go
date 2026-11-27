package sitenav

import (
	"context"
	"testing"
)

func BenchmarkLayeredRoom(b *testing.B) {
	architecture, furniture := semanticFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NewLayeredRoom(architecture, furniture); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSurfaceLocalization(b *testing.B) {
	architecture, furniture := semanticFixture(b)
	model, err := NewLayeredRoom(architecture, furniture)
	if err != nil {
		b.Fatal(err)
	}
	position, rotation := Point{2.1, 3.2}, .7
	cloud := fixtureSurfaceCloud(b, architecture, position, rotation)
	input := RegistrationInput{Cloud: cloud}
	for _, track := range []bool{false, true} {
		name := "Search"
		if track {
			name = "Track"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var fits []RegistrationFit
				var err error
				if track {
					fits, err = TrackSurfaces(context.Background(), model, input, PoseCandidate{Position: position, Rotation: rotation})
				} else {
					fits, err = RegisterSurfaces(context.Background(), model, input)
				}
				if err != nil || len(fits) == 0 {
					b.Fatalf("no match: %v", err)
				}
			}
		})
	}
}
