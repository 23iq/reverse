package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestLogoAnimationFramesKeepDimensions(t *testing.T) {
	widths := []int{120, 44, 18, 7, 1}
	for variant := range logoVariants {
		for _, width := range widths {
			model := newLogoWithVariant(variant)
			model.Width = width

			wantWidth, wantHeight := lipgloss.Size(model.View())
			if wantWidth > width {
				t.Fatalf(
					"variant %q at width %d rendered %d cells wide",
					logoVariants[variant].name,
					width,
					wantWidth,
				)
			}
			for frame := 1; frame < 96; frame++ {
				model.Frame = frame
				gotWidth, gotHeight := lipgloss.Size(model.View())
				if gotWidth != wantWidth || gotHeight != wantHeight {
					t.Fatalf(
						"variant %q frame %d at width %d is %dx%d, want stable %dx%d",
						logoVariants[variant].name,
						frame,
						width,
						gotWidth,
						gotHeight,
						wantWidth,
						wantHeight,
					)
				}
			}
		}
	}
}

func TestLogoCatalogContainsDistinctArtAndMotion(t *testing.T) {
	if len(logoVariants) < 3 {
		t.Fatalf("logo variants = %d, want at least 3", len(logoVariants))
	}

	art := make(map[string]string, len(logoVariants))
	motions := make(map[logoMotion]bool, len(logoVariants))
	for index, variant := range logoVariants {
		model := newLogoWithVariant(index)
		model.Width = 120
		model.Animated = false

		view := model.View()
		if previous, exists := art[view]; exists {
			t.Fatalf("logo variants %q and %q use identical art", previous, variant.name)
		}
		art[view] = variant.name
		motions[variant.motion] = true
	}
	if len(motions) < 3 {
		t.Fatalf("motion variants = %d, want at least 3", len(motions))
	}
}

func TestLogoFramesNeverGainLeadingBounceLines(t *testing.T) {
	for variant := range logoVariants {
		model := newLogoWithVariant(variant)
		model.Width = 120
		for frame := 0; frame < 32; frame++ {
			model.Frame = frame
			view := model.View()
			if view == "" || view[0] == '\n' {
				t.Fatalf(
					"variant %q frame %d starts with a bounce line",
					logoVariants[variant].name,
					frame,
				)
			}
		}
	}
}
