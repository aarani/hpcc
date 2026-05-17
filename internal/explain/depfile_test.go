package explain

import (
	"reflect"
	"testing"
)

func TestParseDepFile(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "single line",
			in:   "foo.o: foo.c bar.h baz.h\n",
			want: []string{"foo.c", "bar.h", "baz.h"},
		},
		{
			name: "continuations",
			in:   "foo.o: foo.c \\\n  bar.h \\\n  baz.h\n",
			want: []string{"foo.c", "bar.h", "baz.h"},
		},
		{
			name: "escaped spaces in path",
			in:   "foo.o: my\\ src.c bar.h\n",
			want: []string{"my src.c", "bar.h"},
		},
		{
			name: "windows path with drive letter",
			in:   "C:\\build\\foo.o: C:\\src\\foo.c C:\\inc\\bar.h\n",
			want: []string{"C:\\src\\foo.c", "C:\\inc\\bar.h"},
		},
		{
			name: "blank lines and comments",
			in:   "\n# comment\nfoo.o: foo.c bar.h\n",
			want: []string{"foo.c", "bar.h"},
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseDepFile([]byte(tc.in))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}
