package terrain

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var adviceCodes = map[string]bool{"terrain.height-gain": true, "terrain.sector-blocked": true, "terrain.no-blockage": true}

var adviceNumRe = regexp.MustCompile(`\d{1,3}(?:,\d{3})+(?:\.\d+)?|\d+(?:\.\d+)?`)

// checkAdviceLines asserts that every summary sentence has a matching
// SummaryItems entry with a documented code and that every number in its
// data is printed in the text (at the text's precision).
func checkAdviceLines(t *testing.T, adv *SiteAdvice) {
	t.Helper()
	if len(adv.SummaryItems) != len(adv.Summary) || len(adv.Summary) == 0 {
		t.Fatalf("%d summary items for %d sentences", len(adv.SummaryItems), len(adv.Summary))
	}
	for i, l := range adv.SummaryItems {
		if l.Text != adv.Summary[i] {
			t.Errorf("item %d text %q != %q", i, l.Text, adv.Summary[i])
		}
		if !adviceCodes[l.Code] || l.Data == nil {
			t.Errorf("item %d code %q data %v", i, l.Code, l.Data)
		}
		var nums [][2]float64
		for _, s := range adviceNumRe.FindAllString(l.Text, -1) {
			s = strings.ReplaceAll(s, ",", "")
			v, _ := strconv.ParseFloat(s, 64)
			dec := 0
			if j := strings.IndexByte(s, '.'); j >= 0 {
				dec = len(s) - j - 1
			}
			nums = append(nums, [2]float64{v, 0.5*math.Pow(10, -float64(dec)) + 1e-9})
		}
		for k, v := range l.Data {
			ok := false
			for _, n := range nums {
				if math.Abs(math.Abs(v)-n[0]) <= n[1] {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s: data.%s = %v not printed in %q", l.Code, k, v, l.Text)
			}
		}
		if l.Code == "terrain.sector-blocked" && (l.Data["distKm"] <= 0 || l.Data["altFt"] != adv.RefAltFt) {
			t.Errorf("sector data %v", l.Data)
		}
	}
}

// A flat site gives the no-blockage line.
func TestAdviceNoBlockageLine(t *testing.T) {
	adv := adviceSummary(&SiteAdvice{RefAltFt: 10000, WorstSectors: nil})
	if len(adv) != 1 || adv[0].Code != "terrain.no-blockage" || adv[0].Data == nil {
		t.Errorf("flat site advice %+v", adv)
	}
}
