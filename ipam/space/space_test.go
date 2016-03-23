package space

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/weaveworks/weave/net/address"
	wt "github.com/weaveworks/weave/testing"
)

func makeSpace(start address.Address, size address.Offset) *Space {
	s := New()
	s.Add(start, size)
	return s
}

func ip(s string) address.Address {
	addr, _ := address.ParseIP(s)
	return addr
}

func emptyCIDR() []address.CIDR {
	return nil
}

func TestLowlevel(t *testing.T) {
	a := []address.Address{}
	a = add(a, 100, 200)
	require.Equal(t, []address.Address{100, 200}, a)
	require.True(t, !contains(a, 99), "")
	require.True(t, contains(a, 100), "")
	require.True(t, contains(a, 199), "")
	require.True(t, !contains(a, 200), "")
	a = add(a, 700, 800)
	require.Equal(t, []address.Address{100, 200, 700, 800}, a)
	a = add(a, 300, 400)
	require.Equal(t, []address.Address{100, 200, 300, 400, 700, 800}, a)
	a = add(a, 400, 500)
	require.Equal(t, []address.Address{100, 200, 300, 500, 700, 800}, a)
	a = add(a, 600, 700)
	require.Equal(t, []address.Address{100, 200, 300, 500, 600, 800}, a)
	a = add(a, 500, 600)
	require.Equal(t, []address.Address{100, 200, 300, 800}, a)
	a = subtract(a, 500, 600)
	require.Equal(t, []address.Address{100, 200, 300, 500, 600, 800}, a)
	a = subtract(a, 600, 700)
	require.Equal(t, []address.Address{100, 200, 300, 500, 700, 800}, a)
	a = subtract(a, 400, 500)
	require.Equal(t, []address.Address{100, 200, 300, 400, 700, 800}, a)
	a = subtract(a, 300, 400)
	require.Equal(t, []address.Address{100, 200, 700, 800}, a)
	a = subtract(a, 700, 800)
	require.Equal(t, []address.Address{100, 200}, a)
	a = subtract(a, 100, 200)
	require.Equal(t, []address.Address{}, a)

	s := New()
	require.Equal(t, address.Count(0), s.NumFreeAddresses())
	ok, got := s.Allocate(address.NewRange(0, 1000))
	require.False(t, ok, "allocate in empty space should fail")

	s.Add(100, 100)
	require.Equal(t, address.Count(100), s.NumFreeAddresses())
	ok, got = s.Allocate(address.NewRange(0, 1000))
	require.True(t, ok && got == 100, "allocate")
	require.Equal(t, address.Count(99), s.NumFreeAddresses())
	require.NoError(t, s.Claim(150))
	require.Equal(t, address.Count(98), s.NumFreeAddresses())
	require.NoError(t, s.Free(100))
	require.Equal(t, address.Count(99), s.NumFreeAddresses())
	wt.AssertErrorInterface(t, (*error)(nil), s.Free(0), "free not allocated")
	wt.AssertErrorInterface(t, (*error)(nil), s.Free(100), "double free")

	r, ok := s.Donate(address.NewRange(0, 1000), false, emptyCIDR)
	require.True(t, ok && r.Start == 125 && r.Size() == 25, "donate")

	// test Donate when addresses are scarce
	s = New()
	r, ok = s.Donate(address.NewRange(0, 1000), false, emptyCIDR)
	require.True(t, !ok, "donate on empty space should fail")
	s.Add(0, 3)
	require.NoError(t, s.Claim(0))
	require.NoError(t, s.Claim(2))
	r, ok = s.Donate(address.NewRange(0, 1000), false, emptyCIDR)
	require.True(t, ok && r.Start == 1 && r.End == 2, "donate")
	r, ok = s.Donate(address.NewRange(0, 1000), false, emptyCIDR)
	require.True(t, !ok, "donate should fail")
}

func TestSpaceAllocate(t *testing.T) {
	const (
		testAddr1   = "10.0.3.4"
		testAddr2   = "10.0.3.5"
		testAddrx   = "10.0.3.19"
		testAddry   = "10.0.9.19"
		containerID = "deadbeef"
		size        = 20
	)
	var (
		start = ip(testAddr1)
	)

	space1 := makeSpace(start, size)
	require.Equal(t, address.Count(20), space1.NumFreeAddresses())
	space1.assertInvariants()

	_, addr1 := space1.Allocate(address.NewRange(start, size))
	require.Equal(t, testAddr1, addr1.String(), "address")
	require.Equal(t, address.Count(19), space1.NumFreeAddresses())
	space1.assertInvariants()

	_, addr2 := space1.Allocate(address.NewRange(start, size))
	require.False(t, addr2.String() == testAddr1, "address")
	require.Equal(t, address.Count(18), space1.NumFreeAddresses())
	require.Equal(t, address.Count(13), space1.NumFreeAddressesInRange(address.Range{Start: ip(testAddr1), End: ip(testAddrx)}))
	require.Equal(t, address.Count(18), space1.NumFreeAddressesInRange(address.Range{Start: ip(testAddr1), End: ip(testAddry)}))
	space1.assertInvariants()

	space1.Free(addr2)
	space1.assertInvariants()

	wt.AssertErrorInterface(t, (*error)(nil), space1.Free(addr2), "double free")
	wt.AssertErrorInterface(t, (*error)(nil), space1.Free(ip(testAddrx)), "address not allocated")
	wt.AssertErrorInterface(t, (*error)(nil), space1.Free(ip(testAddry)), "wrong out of range")

	space1.assertInvariants()
}

func TestSpaceFree(t *testing.T) {
	const (
		testAddr1   = "10.0.3.4"
		testAddrx   = "10.0.3.19"
		testAddry   = "10.0.9.19"
		containerID = "deadbeef"
	)

	entireRange := address.NewRange(ip(testAddr1), 20)
	space := makeSpace(ip(testAddr1), 20)

	// Check we are prepared to give up the entire space
	r := space.biggestFreeRange(entireRange)
	require.True(t, r.Start == ip(testAddr1) && r.Size() == 20, "Wrong space")

	for i := 0; i < 20; i++ {
		ok, _ := space.Allocate(entireRange)
		require.True(t, ok, "Failed to get address")
	}

	// Check we are full
	ok, _ := space.Allocate(entireRange)
	require.True(t, !ok, "Should have failed to get address")
	r, _ = space.Donate(entireRange, false, emptyCIDR)
	require.True(t, r.Size() == 0, "Wrong space")

	// Free in the middle
	require.NoError(t, space.Free(ip("10.0.3.13")))
	r = space.biggestFreeRange(entireRange)
	require.True(t, r.Start == ip("10.0.3.13") && r.Size() == 1, "Wrong space")

	// Free one at the end
	require.NoError(t, space.Free(ip("10.0.3.23")))
	r = space.biggestFreeRange(entireRange)
	require.True(t, r.Start == ip("10.0.3.23") && r.Size() == 1, "Wrong space")

	// Now free a few at the end
	require.NoError(t, space.Free(ip("10.0.3.22")))
	require.NoError(t, space.Free(ip("10.0.3.21")))

	require.Equal(t, address.Count(4), space.NumFreeAddresses())

	// Now get the biggest free space; should be 3.21
	r = space.biggestFreeRange(entireRange)
	require.True(t, r.Start == ip("10.0.3.21") && r.Size() == 3, "Wrong space")

	// Now free a few in the middle
	require.NoError(t, space.Free(ip("10.0.3.12")))
	require.NoError(t, space.Free(ip("10.0.3.11")))
	require.NoError(t, space.Free(ip("10.0.3.10")))

	require.Equal(t, address.Count(7), space.NumFreeAddresses())

	// Now get the biggest free space; should be 3.21
	r = space.biggestFreeRange(entireRange)
	require.True(t, r.Start == ip("10.0.3.10") && r.Size() == 4, "Wrong space")

	require.Equal(t, []address.Range{{Start: ip("10.0.3.4"), End: ip("10.0.3.24")}}, space.OwnedRanges())
}

func TestDonateSimple(t *testing.T) {
	const (
		testAddr1 = "10.0.1.0"
		testAddr2 = "10.0.1.32"
		size      = 48
	)

	var (
		ipAddr1 = ip(testAddr1)
	)

	ps1 := makeSpace(ipAddr1, size)

	// Empty space set should split in two and give me the second half
	r, ok := ps1.Donate(address.NewRange(ip(testAddr1), size), false, emptyCIDR)
	numGivenUp := r.Size()
	require.True(t, ok, "Donate result")
	require.Equal(t, "10.0.1.24", r.Start.String(), "Invalid start")
	require.Equal(t, address.Count(size/2), numGivenUp)
	require.Equal(t, address.Count(size/2), ps1.NumFreeAddresses())

	// Now check we can give the rest up.
	count := 0 // count to avoid infinite loop
	for ; count < 1000; count++ {
		r, ok := ps1.Donate(address.NewRange(ip(testAddr1), size), false, emptyCIDR)
		if !ok {
			break
		}
		numGivenUp += r.Size()
	}
	require.Equal(t, address.Count(0), ps1.NumFreeAddresses())
	require.Equal(t, address.Count(size), numGivenUp)
}

func TestDonateHard(t *testing.T) {
	//common.InitDefaultLogging(true)
	var (
		start                = ip("10.0.1.0")
		size  address.Offset = 48
	)

	// Fill a fresh space
	spaceset := makeSpace(start, size)
	for i := address.Offset(0); i < size; i++ {
		ok, _ := spaceset.Allocate(address.NewRange(start, size))
		require.True(t, ok, "Failed to get IP!")
	}

	require.Equal(t, address.Count(0), spaceset.NumFreeAddresses())

	// Now free all but the last address
	// this will force us to split the free list
	for i := address.Offset(0); i < size-1; i++ {
		require.NoError(t, spaceset.Free(address.Add(start, i)))
	}

	// Now split
	newRange, ok := spaceset.Donate(address.NewRange(start, size), false, emptyCIDR)
	require.True(t, ok, "GiveUpSpace result")
	require.Equal(t, ip("10.0.1.23"), newRange.Start)
	require.Equal(t, address.Count(24), newRange.Size())
	require.Equal(t, address.Count(23), spaceset.NumFreeAddresses())

	//Space set should now have 2 spaces
	expected := New()
	expected.Add(start, 23)
	expected.ours = add(nil, ip("10.0.1.47"), ip("10.0.1.48"))
	require.Equal(t, expected, spaceset)
}

func TestDonateCIDR(t *testing.T) {
	var within address.Range
	var cidrs []address.CIDR

	space1 := New()
	space1.Add(ip("10.0.0.0"), 128)

	// space1 (10.0.0.0/25):
	//  +-------------------------------------------------+
	//  |                       Free					  |
	//  +-------------------------------------------------+
	//  .0                                                .128

	within = addrRange("10.0.0.0", "10.0.0.255")
	cidrs = []address.CIDR{cidr("10.0.0.0/25")}
	space1.Claim(ip("10.0.0.1"))
	space1.Claim(ip("10.0.0.97"))
	chunk1, ok := space1.Donate(within, true, ownedCIDRRanges(cidrs))
	require.True(t, ok, "")
	require.Equal(t, addrRange("10.0.0.32", "10.0.0.63"), chunk1)

	// space1 (10.0.0.0/25):
	//  +-------------------------------------------------+
	//  |  |A|        |Donated|     |A|     Free          |
	//  +-------------------------------------------------+
	//  .0 .1         .32   .64     .97					  .128
	//
	// (A stands for Allocated By Us)

	space1.Claim(ip("10.0.0.2"))
	within = addrRange("10.0.0.0", "10.0.0.3")
	cidrs = []address.CIDR{cidr("10.0.0.0/30")}
	chunk2, ok := space1.Donate(within, true, ownedCIDRRanges(cidrs))
	require.True(t, ok, "")
	require.Equal(t, addrRange("10.0.0.0", "10.0.0.0"), chunk2)

	// space1 (10.0.0.0/30):
	// ~~~~~~~~~~~~~~~~~~~~
	// Allocated: 10.0.0.1, 10.0.0.2
	// Donated: 10.0.0.0
	// Free: 10.0.0.3

	space1.Free(ip("10.0.0.1"))
	within = addrRange("10.0.0.0", "10.0.0.2")
	cidrs = []address.CIDR{cidr("10.0.0.1/32"), cidr("10.0.0.2/32")}
	chunk3, ok := space1.Donate(within, true, ownedCIDRRanges(cidrs))
	require.True(t, ok, "")
	require.Equal(t, addrRange("10.0.0.1", "10.0.0.1"), chunk3)
	_, ok = space1.Donate(within, true, ownedCIDRRanges(cidrs))
	require.False(t, ok, "")

	// Test the donation procedure when there exists completely free CIDR ranges:
	cidrs = []address.CIDR{cidr("10.0.0.0/26"), cidr("10.0.0.128/25")}
	space2 := New()
	space2.Add(ip("10.0.0.0"), 64)
	space2.Add(ip("10.0.0.128"), 128)
	chunk4, ok := space2.Donate(within, true, ownedCIDRRanges(cidrs))
	require.True(t, ok, "")
	require.Equal(t, addrRange("10.0.0.192", "10.0.0.255"), chunk4, "")
}

func TestIsFree(t *testing.T) {
	space1 := New()
	space1.Add(ip("10.0.0.0"), 256)
	require.True(t, space1.IsFree(addrRange("10.0.0.0", "10.0.0.255")))
	require.True(t, space1.IsFree(addrRange("10.0.0.42", "10.0.0.65")))
	space1.Claim(ip("10.0.0.43"))
	require.False(t, space1.IsFree(addrRange("10.0.0.42", "10.0.0.65")))
}

func TestIsFull(t *testing.T) {
	space1 := New()
	space1.Add(ip("10.0.0.0"), 256)
	require.False(t, space1.IsFull(addrRange("10.0.0.0", "10.0.0.255")))
	space1.Claim(ip("10.0.0.43"))
	require.False(t, space1.IsFull(addrRange("10.0.0.0", "10.0.0.255")))
	space1.remove(addrRange("10.0.0.0", "10.0.0.42"))
	require.True(t, space1.IsFull(addrRange("10.0.0.0", "10.0.0.43")))
}

// Helpers

func ownedCIDRRanges(cidrs []address.CIDR) func() []address.CIDR {
	return func() []address.CIDR { return cidrs }
}

func cidr(s string) address.CIDR {
	c, _ := address.ParseCIDR(s)
	return c
}

// Creates [start;end] address.Range
func addrRange(start, end string) address.Range {
	return address.Range{Start: ip(start), End: ip(end) + 1}
}
