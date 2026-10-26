package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"trains/internal/config"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "trains.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func keys(t *testing.T, st *Store) []string {
	t.Helper()
	trains, err := st.Trains(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tr := range trains {
		out = append(out, tr.Key())
	}
	return out
}

func TestTrains(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	weekdays, _ := config.ParseDays([]string{"mon-fri"})
	for _, tr := range []config.Train{
		{Number: 2034, Label: "Morning", Days: weekdays, NotifyAt: "07:20", LateAfter: 3},
		{Number: 652},
		{Number: 2077},
	} {
		if err := st.SaveTrain(ctx, "", tr); err != nil {
			t.Fatal(err)
		}
	}
	trains, _ := st.Trains(ctx)
	if got := trains[0]; got.Label != "Morning" || got.Days.String() != "mon,tue,wed,thu,fri" ||
		got.NotifyAt != "07:20" || got.LateAfter != 3 {
		t.Errorf("first train = %+v", got)
	}

	// Editing, even the key, keeps the train's place.
	if err := st.SaveTrain(ctx, "652", config.Train{Number: 652, Origin: "S09218"}); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, st); len(got) != 3 || got[1] != "652@S09218" {
		t.Errorf("after edit: %v", got)
	}
	if err := st.SaveTrain(ctx, "", config.Train{Number: 2077}); !errors.Is(err, ErrTrainExists) {
		t.Errorf("duplicate add: err = %v", err)
	}
	if err := st.SaveTrain(ctx, "2034", config.Train{Number: 2077}); !errors.Is(err, ErrTrainExists) {
		t.Errorf("edit onto another train: err = %v", err)
	}
	if err := st.DeleteTrain(ctx, "2034"); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, st); len(got) != 2 || got[0] != "652@S09218" {
		t.Errorf("after delete: %v", got)
	}
}

func TestImportTrainsOnce(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	if ok, err := st.ImportTrains(ctx, []config.Train{{Number: 1}, {Number: 2}}); !ok || err != nil {
		t.Fatalf("first import: %v, %v", ok, err)
	}
	st.DeleteTrain(ctx, "1")
	if ok, _ := st.ImportTrains(ctx, []config.Train{{Number: 1}, {Number: 3}}); ok {
		t.Error("imported a second time")
	}
	if got := keys(t, st); len(got) != 1 || got[0] != "2" {
		t.Errorf("trains = %v, want just 2", got)
	}
}
