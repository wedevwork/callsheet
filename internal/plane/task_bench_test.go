package plane

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/wedevwork/callsheet/internal/contract"
)

// BenchmarkTaskAdmission measures the admission decision over 100 roles
// of one name: the coherent registry/node/task observation, candidate
// reasons and the first-eligible pick, when the first, only the last, or
// no role is eligible. No filesystem is touched.
func BenchmarkTaskAdmission(b *testing.B) {
	for _, c := range []struct {
		name string
		full int // leading roles at capacity
		want int // picked index, -1 for none
	}{{"first", 0, 0}, {"last", 99, 99}, {"none", 100, -1}} {
		b.Run(c.name, func(b *testing.B) {
			root := filepath.Join(b.TempDir(), "state")
			os.MkdirAll(filepath.Join(root, nodesName), 0o700)
			os.WriteFile(filepath.Join(root, nodesName, idA+".json"), encodeNodeRecord(idA, t0), 0o600)
			var recs []contract.RoleRecord
			for i := 0; i < 100; i++ {
				recs = append(recs, record(taskCfg("role-"+strconv.Itoa(i), "coder", idA, 1), i+1))
			}
			doc := docOf(1, 101, recs...)
			r, err := loadNodeRegistry(layout{root: root}, realClock{})
			if err != nil {
				b.Fatal(err)
			}
			r.roles = newRoleRegistry(layout{root: root}, defaultDeps(), roleLookup, doc)
			gen, err := r.attach(idA, "bench", contract.ProtocolVersion, nil, &nodeStream{})
			if err != nil {
				b.Fatal(err)
			}
			snap := doc.snapshotFor(idA)
			r.ackSnapshot(idA, gen, snap)
			hb := contract.HeartbeatBody{RolesRevision: snap.rev}
			for _, rec := range snap.roles {
				hb.Roles = append(hb.Roles, contract.RoleStatus{RoleID: rec.ID, Concurrency: 1, CanAccept: true})
			}
			if err := r.heartbeat(idA, gen, hb); err != nil {
				b.Fatal(err)
			}
			ts := &taskService{reg: r, roles: r.roles, counts: map[instanceKey]*heldCount{}, blocked: map[*taskEntry]bool{}}
			for i := 0; i < c.full; i++ {
				ts.counts[keyOf(recs[i])] = &heldCount{held: 1}
			}
			target := contract.TaskTarget{Kind: contract.TargetName, Value: "coder"}
			b.ReportAllocs()
			for b.Loop() {
				ts.mu.Lock()
				ob := ts.observeLocked(target)
				ts.mu.Unlock()
				pick := -1
				for i, cand := range ob.cands {
					if cand.CanAccept {
						pick = i
						break
					}
				}
				if pick != c.want || len(ob.cands) != 100 || (pick >= 0 && ob.gens[pick] != gen) {
					b.Fatalf("picked %d of %d, want %d", pick, len(ob.cands), c.want)
				}
			}
		})
	}
}

// BenchmarkTaskCheckpoint measures one durable task-document publication
// (encode with the base64 tail, temporary write and fsync, atomic rename,
// directory sync) with 0, 64 KiB and 10 MiB of retained output, checking
// the 16 MiB document bound and the strict round trip.
func BenchmarkTaskCheckpoint(b *testing.B) {
	for _, size := range []int{0, 64 << 10, contract.MaxLogRetainedBytes} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			root := filepath.Join(b.TempDir(), "state")
			os.MkdirAll(root, 0o700)
			st := &taskStore{l: layout{root: root}, d: defaultDeps()}
			start := t0.Add(time.Second)
			rec := contract.TaskRecord{TaskID: "t_00000000000000000000000000000001", Request: taskReq(contract.TargetID, "a", "bench"),
				Role: record(taskCfg("a", "coder", idA, 1), 1), Effective: contract.TaskEffective{Model: "m", Effort: "low", Timeout: time.Hour},
				Execution: contract.ExecutionToken{Epoch: "00000000000000000000000000000000", Attachment: 1}, State: contract.TaskRunning,
				CreatedAt: t0, StartedAt: &start, Revision: 1,
				Log: contract.TaskLog{Data: bytes.Repeat([]byte{0x1b, 'x', '\n'}, size/3+1)[:size], SourceBytes: size, ReceivedBytes: size}}
			tmp, err := st.prepare(rec)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := st.publishFirst(tmp, rec.TaskID); err != nil {
				b.Fatal(err)
			}
			enc, err := encodeTask(rec)
			if err != nil || len(enc) > maxTaskFile {
				b.Fatalf("document %d bytes: %v", len(enc), err)
			}
			b.SetBytes(int64(len(enc)))
			b.ReportAllocs()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			n := 0
			for b.Loop() {
				n++
				rec.Revision++
				if err := st.update(rec); err != nil {
					b.Fatal(err)
				}
			}
			runtime.ReadMemStats(&after)
			// Bounded encoding: about one encoded document per write, far
			// below the 64 MiB per-task allowance.
			if per := (after.TotalAlloc - before.TotalAlloc) / uint64(max(n, 1)); per > uint64(len(enc))+1<<20 {
				b.Fatalf("%d bytes allocated per write of a %d byte document", per, len(enc))
			}
			got, err := st.l.readTaskFile(rec.TaskID, roleLookup)
			if err != nil || got.Revision != rec.Revision || !bytes.Equal(got.Log.Data, rec.Log.Data) {
				b.Fatalf("round trip: %v", err)
			}
		})
	}
}
