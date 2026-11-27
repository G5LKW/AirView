package refdata

// militaryBlocks are publicly documented address ranges that states use for
// military (or other state) aircraft. Sorted, non-overlapping.
//
// HEURISTIC. Only a few states set aside a recognisable block, the edges are
// established by observation rather than published by ICAO, some blocks
// contain a handful of civil airframes, and many military aircraft (and all of
// the states not listed) use ordinary addresses. Treat the result as a hint:
// "probably military", never as proof either way. The ranges are the ones
// maintained by the tar1090-db project (ranges.json).
var militaryBlocks = []addrBlock{
	{0x010070, 0x01008F, "Egypt", "EG"},
	{0x0A4000, 0x0A4FFF, "Algeria", "DZ"},
	{0x33FF00, 0x33FFFF, "Italy", "IT"},
	{0x350000, 0x37FFFF, "Spain", "ES"},
	{0x3AA000, 0x3AFFFF, "France", "FR"},
	{0x3B7000, 0x3BFFFF, "France", "FR"},
	{0x3EA000, 0x3EBFFF, "Germany", "DE"},
	{0x3F4000, 0x3FBFFF, "Germany", "DE"},
	{0x400000, 0x40003F, "United Kingdom", "GB"},
	{0x43C000, 0x43CFFF, "United Kingdom", "GB"},
	{0x444000, 0x446FFF, "Austria", "AT"},
	{0x44F000, 0x44FFFF, "Belgium", "BE"},
	{0x457000, 0x457FFF, "Bulgaria", "BG"},
	{0x45F400, 0x45F4FF, "Denmark", "DK"},
	{0x468000, 0x4683FF, "Greece", "GR"},
	{0x473C00, 0x473C0F, "Hungary", "HU"},
	{0x478100, 0x4781FF, "Norway", "NO"},
	{0x480000, 0x480FFF, "Netherlands", "NL"},
	{0x48D800, 0x48D87F, "Poland", "PL"},
	{0x497C00, 0x497CFF, "Portugal", "PT"},
	{0x498420, 0x49842F, "Czechia", "CZ"},
	{0x4B7000, 0x4B7FFF, "Switzerland", "CH"},
	{0x4B8200, 0x4B82FF, "Turkey", "TR"},
	{0x70C070, 0x70C07F, "Oman", "OM"},
	{0x710258, 0x71028F, "Saudi Arabia", "SA"},
	{0x710380, 0x71039F, "Saudi Arabia", "SA"},
	{0x738A00, 0x738AFF, "Israel", "IL"},
	{0x7CF800, 0x7CFAFF, "Australia", "AU"},
	{0x800200, 0x8002FF, "India", "IN"},
	{0xADF7C8, 0xAFFFFF, "United States", "US"},
	{0xC20000, 0xC3FFFF, "Canada", "CA"},
	{0xE40000, 0xE41FFF, "Brazil", "BR"},
}

// IsMilitary reports whether the address lies in one of the publicly
// documented military address blocks.
//
// This is a heuristic (see militaryBlocks): a true result means "very likely a
// military / state aircraft", a false result means nothing. It does not
// allocate.
func IsMilitary(icao uint32) bool {
	return icao <= 0xFFFFFF && findBlock(militaryBlocks, icao) >= 0
}
