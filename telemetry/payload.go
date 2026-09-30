package telemetry

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"time"

	"github.com/jamestryand/pocketcqrs/opsport"
)

// TimestampFormat is the format sent_at uses: UTC RFC 3339 with milliseconds.
const TimestampFormat = "2006-01-02T15:04:05.000Z"

// Payload is the telemetry snapshot (health/telemetry contract section 8.3):
// one UTF-8 JSON object with the same figures /metrics would return at that
// moment. series has an entry for every series of section 6; a gauge or
// counter sample is {"labels": {...}, "value": n} and a histogram sample
// {"labels": {...}, "buckets": {"<le>": n, ...}, "sum": n, "count": n}. A
// value that is not known (NaN in /metrics) is null.
func Payload(snap opsport.Snapshot, nodeID, role string, sentAt time.Time) []byte {
	var b bytes.Buffer
	str := func(s string) { j, _ := json.Marshal(s); b.Write(j) }
	num := func(v float64) {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			b.WriteString("null")
			return
		}
		b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	}
	labels := func(ls []opsport.Label) {
		b.WriteString(`"labels":{`)
		for i, l := range ls {
			if i > 0 {
				b.WriteByte(',')
			}
			str(l.Name)
			b.WriteByte(':')
			str(l.Value)
		}
		b.WriteByte('}')
	}

	b.WriteString(`{"contract_version":`)
	str(opsport.ContractVersion)
	b.WriteString(`,"node_id":`)
	str(nodeID)
	b.WriteString(`,"role":`)
	str(role)
	b.WriteString(`,"sent_at":`)
	str(sentAt.UTC().Format(TimestampFormat))
	b.WriteString(`,"series":{`)
	for i, f := range snap.Families {
		if i > 0 {
			b.WriteByte(',')
		}
		str(f.Name)
		b.WriteString(":[")
		first := true
		sep := func() {
			if !first {
				b.WriteByte(',')
			}
			first = false
		}
		for _, s := range f.Samples {
			sep()
			b.WriteByte('{')
			labels(s.Labels)
			b.WriteString(`,"value":`)
			num(s.Value)
			b.WriteByte('}')
		}
		for _, h := range f.Histograms {
			sep()
			b.WriteByte('{')
			labels(h.Labels)
			b.WriteString(`,"buckets":{`)
			for j, le := range opsport.DurationBuckets {
				if j > 0 {
					b.WriteByte(',')
				}
				str(opsport.Number(le))
				b.WriteByte(':')
				b.WriteString(strconv.FormatInt(h.Buckets[j], 10))
			}
			b.WriteString(`,"+Inf":`)
			b.WriteString(strconv.FormatInt(h.Count, 10))
			b.WriteString(`},"sum":`)
			num(h.Sum)
			b.WriteString(`,"count":`)
			b.WriteString(strconv.FormatInt(h.Count, 10))
			b.WriteByte('}')
		}
		b.WriteByte(']')
	}
	b.WriteString("}}")
	return b.Bytes()
}
