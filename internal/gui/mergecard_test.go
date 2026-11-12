package gui

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/ui"
	"github.com/tyler-builds/jamb/ui/uitest"

	"github.com/tyler-builds/solo/internal/core"
	"github.com/tyler-builds/solo/internal/forge"
	"github.com/tyler-builds/solo/internal/store"
)

const (
	formsID     = "c3d4e5f6-forms"
	longReposID = "l1l2l3l4-longrepos"
	longPRsID   = "l5l6l7l8-longprs"
	longNameID  = "l9l0l1l2-longname"
)

var longRepoNames = []string{"inventory_service_core", "inventory-service-public-api", "inventory-service-admin-web"}

// longReposThread is a settled thread across three repositories with long names, ready to open its
// merge requests.
func longReposThread(id string, names []string) core.ThreadView {
	now := time.Now()
	parts := make([]core.PartView, len(names))
	for i, n := range names {
		parts[i] = core.PartView{Part: core.Part{Repo: "/home/dev/code/" + n, Branch: "feature/INV-2041-warehouse-filter", BaseBranch: "main"},
			Name: n, Changed: 3 + i, Forge: forge.GitLab, CanOpenPR: true}
	}
	return core.ThreadView{Thread: store.Thread{ID: id, Kind: store.KindWorker,
		Title: "INV-2041: Add a warehouse filter to the stock level dropdowns, banner and search", Status: core.StatusIdle,
		RepoPath: parts[0].Repo, Branch: parts[0].Branch, BaseBranch: "main", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute)},
		IsWorker: true, HasWorktree: true, Changed: 12, Forge: forge.GitLab, CanOpenPR: true, CanDone: true, CanDiscard: true, Parts: parts}
}

// longPRsThread is longReposThread with a pull request open in each repository, its checks running.
func longPRsThread() core.ThreadView {
	th := longReposThread(longPRsID, longRepoNames)
	th.Forge, th.CanOpenPR = forge.GitHub, false
	for i := range th.Parts {
		p := &th.Parts[i]
		p.Forge, p.CanOpenPR = forge.GitHub, false
		p.PR = checksPR(300+i, forge.CheckCounts{Total: 4, Passed: 3, Pending: 1})
	}
	return th
}

// mergeCardUI shows Solo's chat at width w with only a merge card for the thread, after the four
// steps Solo took to check it.
func mergeCardUI(t *testing.T, id string, w float32) (*App, *uitest.UI) {
	t.Helper()
	f := demoBackend()
	f.threads = append(f.threads, longReposThread(longReposID, longRepoNames), longPRsThread(),
		longReposThread(longNameID, []string{"inventory-service-administration-console-and-reporting-web-frontend-for-the-warehouse-and-stock-teams"}))
	o := core.OrchestratorID
	f.items[o] = nil
	f.add(o, core.ItemUser, core.UserData{Text: "How is the warehouse filter coming along?"})
	for _, s := range []string{"t1", "t2", "t3", "t4"} {
		f.add(o, core.ItemTool, tool(s, "mcp__solo__get_diff", map[string]any{"id": id[:8]}, "diff --git …"))
	}
	f.add(o, core.ItemProposal, core.ProposalData{ThreadID: id,
		Summary: "Adds a warehouse-only list (warehouses plus an \"All\" row with sentinel ID 255) to the stock lookup. The API exposes it, and only the stock level dropdowns, banner, and search use it. The Get, Update and Search procs map 255 to and from \"All\". API build and the non-DB tests pass; lint is clean."})
	_, dark := Themes()
	a := New(f)
	u := uitest.New(t, a.Root, uitest.Size(w, 900), uitest.Theme(dark), uitest.Bindings(Bindings()...))
	t.Cleanup(u.Close)
	u.Settle()
	return a, u
}

// repoTexts is the repositories the card's header shows, and its "+N" if any.
func repoTexts(u *uitest.UI, id string) (shown []string, more string) {
	for i := 0; u.Has(cardRepoKey(id, i)); i++ {
		shown = append(shown, u.Get(cardRepoKey(id, i)).Text())
	}
	if u.Has("card-repos-more-" + id) {
		more = u.Get("card-repos-more-" + id).Text()
	}
	return shown, more
}

func cardRepoKey(id string, i int) string { return fmt.Sprintf("card-repo-%s-%d", id, i) }

// The merge card's header puts "Merge proposed" and its checks at the top left, the repositories at
// the top right, and the title on a line of its own under them, wide and narrow. Repositories that
// don't fit whole end the row as "+N"; the ones shown are never squeezed.
func TestMergeCardHeader(t *testing.T) {
	for _, c := range []struct {
		name  string
		id    string
		w     float32
		shown int // repositories named; the rest are "+N"
	}{
		{"one short repo", formsID, 1440, 1},
		{"three long repos", longReposID, 1440, 3},
		{"three long repos, narrow", longReposID, 860, 2},
		{"one short repo, narrow", formsID, 860, 1},
		{"three long repos with checks", longPRsID, 1440, 2},
		{"three long repos with checks, narrow", longPRsID, 860, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, u := mergeCardUI(t, c.id, c.w)
			card := u.Get("proposal-" + c.id).Bounds()
			repos := u.Get("card-repos-" + c.id).Bounds()
			title := u.Get("card-title-" + c.id).Bounds()
			kind := u.Get("card-kind-" + c.id).Bounds()

			if right, want := repos.X+repos.W, card.X+card.W-space4-1; right < want-2 || right > want+0.5 {
				t.Errorf("repositories end at %v, want at the card's right edge %v", right, want)
			}
			if d := (repos.Y + repos.H/2) - (kind.Y + kind.H/2); d < -2 || d > 2 {
				t.Errorf("repositories %v not on the row of Merge proposed %v", repos, kind)
			}
			if gap := repos.X - (kind.X + kind.W); gap < space3-0.5 {
				t.Errorf("repositories %v crowd Merge proposed and its checks %v: %v apart", repos, kind, gap)
			}
			if title.Y < kind.Y+kind.H || title.Y < repos.Y+repos.H {
				t.Errorf("title %v not under the top row (%v, %v)", title, kind, repos)
			}
			if left := title.X - card.X; left > space4+2 {
				t.Errorf("title starts %v into the card, want at its left edge", left)
			}
			if !strings.Contains(u.Get("card-title-"+c.id).Name(), "INV-2041") && c.id != formsID {
				t.Errorf("title %q", u.Get("card-title-"+c.id).Name())
			}

			shown, more := repoTexts(u, c.id)
			if len(shown) != c.shown {
				t.Errorf("shows %d repositories %q, want %d", len(shown), shown, c.shown)
			}
			total := 1
			if c.id != formsID {
				total = len(longRepoNames)
			}
			if rest := total - len(shown); rest > 0 {
				if want := fmt.Sprintf("+%d", rest); more != want {
					t.Errorf("hidden repositories shown as %q, want %q", more, want)
				}
			} else if more != "" {
				t.Errorf("all repositories shown, and %q too", more)
			}
			for i, n := range shown {
				if c.id != formsID && n != longRepoNames[i] {
					t.Errorf("repository %d shows %q, want it whole: %q", i, n, longRepoNames[i])
				}
			}
			// A hidden name wouldn't have fit in the room left of the row.
			if len(shown) < total {
				next := textWidth(nil, longRepoNames[len(shown)], 12, ui.FontRegular)
				if free := repos.X - (kind.X + kind.W) - space3; next < free {
					t.Errorf("%q (%v wide) hidden with %v free", longRepoNames[len(shown)], next, free)
				}
			}
			// Whole names are drawn whole: as wide as they measure.
			for i := range shown {
				b := u.Get(cardRepoKey(c.id, i)).Bounds()
				if want := textWidth(nil, shown[i], 12, ui.FontRegular); b.W < want-1 {
					t.Errorf("repository %q is %v wide, squeezed from %v", shown[i], b.W, want)
				}
			}
		})
	}
}

// tooltipOn hovers the element and returns the tooltip it shows.
func tooltipOn(t *testing.T, u *uitest.UI, key string) string {
	t.Helper()
	u.Get(key).Hover()
	for i := 0; i < 15; i++ {
		u.Advance(100 * time.Millisecond)
	}
	u.Settle()
	tip := ""
	u.Tree().Walk(func(n *access.Node) bool {
		if n.Role == access.RoleTooltip {
			tip = n.Name
		}
		return tip == ""
	})
	return tip
}

// Hovering "+N" lists the repositories it stands for, and a name cut short shows in full.
func TestMergeCardRepoTooltips(t *testing.T) {
	_, u := mergeCardUI(t, longReposID, 860)
	tip := tooltipOn(t, u, "card-repos-more-"+longReposID)
	if tip != longRepoNames[2] {
		t.Errorf("+1's tooltip %q, want %q", tip, longRepoNames[2])
	}

	_, u = mergeCardUI(t, longNameID, 860)
	key := cardRepoKey(longNameID, 0)
	long := "inventory-service-administration-console-and-reporting-web-frontend-for-the-warehouse-and-stock-teams"
	if b, card := u.Get(key).Bounds(), u.Get("proposal-"+longNameID).Bounds(); b.X+b.W > card.X+card.W {
		t.Errorf("the long name %v overflows the card %v", b, card)
	}
	if tip := tooltipOn(t, u, key); tip != long {
		t.Errorf("the cut name's tooltip %q, want %q", tip, long)
	}
}

// TestMergeCardShots renders the merge card for review:
//
//	SOLO_CARD_SHOTS=~/Pictures/solo-merge-card go test ./internal/gui -run TestMergeCardShots
func TestMergeCardShots(t *testing.T) {
	out := os.Getenv("SOLO_CARD_SHOTS")
	if out == "" {
		t.Skip("set SOLO_CARD_SHOTS to an output directory")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	var crops []image.Image
	for _, c := range []struct {
		name string
		id   string
		w    float32
	}{
		{"1-short-repo", formsID, 1440},
		{"2-long-repos", longReposID, 1440},
		{"3-long-repos-narrow", longReposID, 860},
		{"4-short-repo-narrow", formsID, 860},
		{"5-checks", longPRsID, 1440},
		{"6-checks-narrow", longPRsID, 860},
	} {
		_, u := mergeCardUI(t, c.id, c.w)
		for i := 0; i < 10; i++ {
			u.Advance(100 * time.Millisecond)
		}
		u.Settle()
		crop := cropTo(u.Screenshot(), u.Get("proposal-"+c.id).Bounds(), 12)
		crops = append(crops, crop)
		savePNG(t, filepath.Join(out, "final-"+c.name+".png"), crop)
		if c.name == "3-long-repos-narrow" {
			u.Get("card-repos-more-" + c.id).Hover()
			for i := 0; i < 15; i++ {
				u.Advance(100 * time.Millisecond)
			}
			u.Settle()
			b := u.Get("proposal-" + c.id).Bounds()
			b.W += 120 // the tooltip hangs past the card's right edge
			savePNG(t, filepath.Join(out, "final-"+c.name+"-hover.png"), cropTo(u.Screenshot(), b, 12))
		}
	}
	savePNG(t, filepath.Join(out, "sheet-final.png"), stack(crops, 24))
}

// cropTo cuts the shot to r with pad around it, in the shot's pixels.
func cropTo(img image.Image, r ui.Rect, pad float32) image.Image {
	rect := image.Rect(int(r.X-pad), int(r.Y-pad), int(r.X+r.W+pad), int(r.Y+r.H+pad)).Intersect(img.Bounds())
	dst := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(dst, dst.Bounds(), img, rect.Min, draw.Src)
	return dst
}

// stack puts the images one above the other, gap apart, on a grey ground.
func stack(imgs []image.Image, gap int) image.Image {
	w, h := 0, gap
	for _, im := range imgs {
		w = max(w, im.Bounds().Dx())
		h += im.Bounds().Dy() + gap
	}
	dst := image.NewRGBA(image.Rect(0, 0, w+2*gap, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.RGBA{40, 40, 44, 255}), image.Point{}, draw.Src)
	y := gap
	for _, im := range imgs {
		draw.Draw(dst, image.Rect(gap, y, gap+im.Bounds().Dx(), y+im.Bounds().Dy()), im, image.Point{}, draw.Src)
		y += im.Bounds().Dy() + gap
	}
	return dst
}

func savePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
