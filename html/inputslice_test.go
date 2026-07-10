package html

import (
	"testing"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/test"
)

func TestInputSlice(t *testing.T) {
	// Spare capacity so NewInputBytes plants a temporary NULL without realloc.
	buf := make([]byte, 10, 20)
	copy(buf, "abcdefghij")
	z := parse.NewInputBytes(buf)
	defer z.Restore()

	full := z.Bytes()
	test.That(t, len(full) == 10)

	// Zero-capped token as Shift would return: "cde" at offset 2.
	data := full[2:5:5]
	test.That(t, string(data) == "cde")
	test.That(t, cap(data) == 3)

	got := inputSlice(z, 2, data)
	test.That(t, string(got) == "cde")
	// Cap extends to end of document (not past the NULL sentinel).
	test.That(t, cap(got) == len(full)-2)
	test.That(t, &got[0] == &full[2])

	// Empty data: returned as-is.
	empty := inputSlice(z, 0, nil)
	test.That(t, len(empty) == 0)
	empty2 := inputSlice(z, 0, []byte{})
	test.That(t, len(empty2) == 0)

	// Offset out of range: fall back to data unchanged.
	oob := inputSlice(z, 100, data)
	test.That(t, &oob[0] == &data[0])
	test.That(t, cap(oob) == cap(data))

	neg := inputSlice(z, -1, data)
	test.That(t, &neg[0] == &data[0])

	// offset+len exceeds full: fall back.
	tooLong := inputSlice(z, 8, data) // 8+3 > 10
	test.That(t, &tooLong[0] == &data[0])

	// Address mismatch: same length/content, different backing.
	other := []byte("cde")
	mismatch := inputSlice(z, 2, other)
	test.That(t, &mismatch[0] == &other[0])
	test.That(t, cap(mismatch) == cap(other))
}
