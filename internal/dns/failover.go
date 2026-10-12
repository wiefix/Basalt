package dns

// Failover drops from z the A and AAAA records whose health check is down.
// A set (name and type) whose every record would go keeps them all: when
// every address fails the check, answering with all of them is better than
// answering with none (the check may be what is broken). Records without a
// check always stay.
func Failover(z *Zone, down func(check int64) bool) {
	type set struct{ name, typ string }
	alive := map[set]bool{}
	for i := range z.Records {
		r := &z.Records[i]
		if r.HealthCheck == 0 || !down(r.HealthCheck) {
			alive[set{r.Name, r.Type}] = true
		}
	}
	out := z.Records[:0:0]
	for _, r := range z.Records {
		if r.HealthCheck > 0 && down(r.HealthCheck) && alive[set{r.Name, r.Type}] {
			continue
		}
		out = append(out, r)
	}
	z.Records = out
}
