package replicate

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	"go.podman.io/storage"
)

// fakeResolver resolves image lookups by ID or by name against a fixed set of
// images, mimicking storage.Store.Image: an unknown key yields
// storage.ErrImageUnknown.
type fakeResolver struct {
	images []storage.Image
}

func (f *fakeResolver) GraphRoot() string { return "/fake/graphroot" }

func (f *fakeResolver) Image(id string) (*storage.Image, error) {
	for i := range f.images {
		if f.images[i].ID == id {
			return &f.images[i], nil
		}
		for _, name := range f.images[i].Names {
			if name == id {
				return &f.images[i], nil
			}
		}
	}
	return nil, storage.ErrImageUnknown
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAvailableNames(t *testing.T) {
	tests := []struct {
		name    string
		dst     []storage.Image
		img     storage.Image
		want    []string
		wantErr bool
	}{
		{
			name: "no conflict keeps all names",
			dst:  nil,
			img:  storage.Image{ID: "new", Names: []string{"localhost/app:latest", "localhost/app:v2"}},
			want: []string{"localhost/app:latest", "localhost/app:v2"},
		},
		{
			name: "name held by a different image is dropped",
			dst:  []storage.Image{{ID: "old", Names: []string{"localhost/app:latest"}}},
			img:  storage.Image{ID: "new", Names: []string{"localhost/app:latest"}},
			want: nil,
		},
		{
			name: "colliding name dropped, free name kept",
			dst:  []storage.Image{{ID: "old", Names: []string{"localhost/app:latest"}}},
			img:  storage.Image{ID: "new", Names: []string{"localhost/app:latest", "localhost/app:v2"}},
			want: []string{"localhost/app:v2"},
		},
		{
			name: "name already on the same image id is kept",
			dst:  []storage.Image{{ID: "new", Names: []string{"localhost/app:latest"}}},
			img:  storage.Image{ID: "new", Names: []string{"localhost/app:latest"}},
			want: []string{"localhost/app:latest"},
		},
		{
			name: "image with no names yields no names",
			dst:  []storage.Image{{ID: "old", Names: []string{"localhost/app:latest"}}},
			img:  storage.Image{ID: "new"},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeResolver{images: tt.dst}
			got, err := availableNames(r, &tt.img, discardLogger())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("availableNames = %v, want %v", got, tt.want)
			}
		})
	}
}
