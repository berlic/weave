package address

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func ip(s string) Address {
	addr, _ := ParseIP(s)
	return addr
}

func cidr(s string) CIDR {
	c, _ := ParseCIDR(s)
	return c
}

func TestCIDRs(t *testing.T) {
	start := ip("192.168.1.42")
	end := ip("192.168.2.42")
	offset := Subtract(end, start)
	r := NewRange(start, offset)
	// for [192.168.1.42,192.168.2.42)
	expectedCIDRs := []CIDR{
		{ip("192.168.1.42"), 31},
		{ip("192.168.1.44"), 30},
		{ip("192.168.1.48"), 28},
		{ip("192.168.1.64"), 26},
		{ip("192.168.1.128"), 25},
		{ip("192.168.2.0"), 27},
		{ip("192.168.2.32"), 29},
		{ip("192.168.2.40"), 31},
	}
	cidrs := r.CIDRs()

	require.Equal(t, len(cidrs), len(expectedCIDRs), "")
	require.Equal(t, expectedCIDRs, cidrs, "")
}

func TestSingleCIDR(t *testing.T) {
	r := NewRange(ip("192.168.1.0"), 256)
	expectedCIDR := CIDR{ip("192.168.1.0"), 24}
	cidrs := r.CIDRs()

	require.Equal(t, len(cidrs), 1)
	require.Equal(t, expectedCIDR, cidrs[0])

	r = NewRange(ip("192.168.1.1"), 1)
	expectedCIDR = CIDR{ip("192.168.1.1"), 32}
	cidrs = r.CIDRs()

	require.Equal(t, len(cidrs), 1)
	require.Equal(t, expectedCIDR, cidrs[0])

	r = Range{ip("10.0.0.2"), ip("10.0.0.9")}
	expectedCIDRs := []CIDR{cidr("10.0.0.2/31"), cidr("10.0.0.4/30"), cidr("10.0.0.8/32")}
	require.Equal(t, expectedCIDRs, r.CIDRs(), "")
}

func TestIsCIDR(t *testing.T) {
	require.True(t, NewRange(ip("10.20.0.0"), 256).IsCIDR(), "")
	require.True(t, NewRange(ip("10.20.0.1"), 1).IsCIDR(), "")
	require.False(t, NewRange(ip("10.20.0.1"), 2).IsCIDR(), "")
	require.False(t, NewRange(ip("10.20.0.0"), 254).IsCIDR(), "")
	require.True(t, NewRange(ip("10.0.0.0"), 4).IsCIDR(), "")
}

func TestHalve(t *testing.T) {
	cidr0, _ := ParseCIDR("10.0.0.1/32")
	_, _, ok := cidr0.Halve()
	require.False(t, ok, "")

	cidr1, _ := ParseCIDR("10.0.0.0/24")
	a, b, ok := cidr1.Halve()
	require.True(t, ok, "")
	require.Equal(t, "10.0.0.0/25", a.String(), "")
	require.Equal(t, "10.0.0.128/25", b.String(), "")
}

func TestCIDRStartAndEnd(t *testing.T) {
	cidr, _ := ParseCIDR("10.0.0.0/24")
	require.Equal(t, ip("10.0.0.0"), cidr.Start(), "")
	require.Equal(t, ip("10.0.0.255"), cidr.End(), "")
}
