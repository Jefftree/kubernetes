package podproto

import (
	"fmt"
	goruntime "runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBreakdownWhereThe70Go(t *testing.T) {
	const n = 10000
	base := loadExemplar(t)
	t.Logf("exemplar: %d labels, %d annotations, %d ownerRefs, %d finalizers, %d managedFields entries",
		len(base.Labels), len(base.Annotations), len(base.OwnerReferences), len(base.Finalizers), len(base.ManagedFields))

	raws := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		p := base.DeepCopy()
		p.Namespace, p.Name = "ns", fmt.Sprintf("pod-%06d", i)
		p.Spec.NodeName = fmt.Sprintf("node-%d", i%5000)
		b, _ := p.Marshal()
		e := make([]byte, len(b))
		copy(e, b)
		raws = append(raws, e)
	}

	measure := func(build func() any) (int64, int64) {
		b0, o0 := liveHeap()
		held := build()
		b1, o1 := liveHeap()
		goruntime.KeepAlive(held)
		return int64(b1) - int64(b0), int64(o1) - int64(o0)
	}

	// Full Element, as the spike builds it.
	fb, fo := measure(func() any {
		out := make([]*Element, 0, n)
		for i, r := range raws {
			e, _ := Build(r, fmt.Sprint(i))
			tail := make([]byte, len(e.Tail))
			copy(tail, e.Tail)
			e.Tail = tail
			out = append(out, e)
		}
		return out
	})

	// Just the tail bytes.
	tb, to := measure(func() any {
		out := make([][]byte, 0, n)
		for _, r := range raws {
			_, _, _, tail, _ := SplitPod(r)
			c := make([]byte, len(tail))
			copy(c, tail)
			out = append(out, c)
		}
		return out
	})

	// Just the attrs (labels + fields sets), which upstream also pays.
	ab, ao := measure(func() any {
		out := make([]*Element, 0, n)
		for i, r := range raws {
			e, _ := Build(r, fmt.Sprint(i))
			out = append(out, &Element{Labels: e.Labels, Fields: e.Fields})
		}
		return out
	})

	// Decoded ObjectMeta with individual fields dropped, to see what each costs.
	metaVariant := func(strip func(*metav1.ObjectMeta)) (int64, int64) {
		return measure(func() any {
			out := make([]*metav1.ObjectMeta, 0, n)
			for _, r := range raws {
				meta, _, _, _, _ := SplitPod(r)
				om := &metav1.ObjectMeta{}
				_ = om.Unmarshal(meta)
				strip(om)
				out = append(out, om)
			}
			return out
		})
	}
	wb, wo := metaVariant(func(om *metav1.ObjectMeta) {})
	nomfB, nomfO := metaVariant(func(om *metav1.ObjectMeta) { om.ManagedFields = nil })
	nolabB, nolabO := metaVariant(func(om *metav1.ObjectMeta) { om.Labels = nil })
	noannB, noannO := metaVariant(func(om *metav1.ObjectMeta) { om.Annotations = nil })
	noownB, noownO := metaVariant(func(om *metav1.ObjectMeta) { om.OwnerReferences = nil })
	bareB, bareO := metaVariant(func(om *metav1.ObjectMeta) {
		om.ManagedFields, om.Labels, om.Annotations, om.OwnerReferences, om.Finalizers = nil, nil, nil, nil, nil
	})

	row := func(name string, b, o int64) {
		t.Logf("  %-42s %6.1f MB %9d objs  %6.1f objs/pod", name, float64(b)/(1<<20), o, float64(o)/n)
	}
	t.Logf("TOTAL");            row("full Element (the spike)", fb, fo)
	t.Logf("COMPONENTS");       row("tail bytes only", tb, to)
	row("labels + fields sets only (upstream pays too)", ab, ao)
	row("decoded ObjectMeta, whole", wb, wo)
	t.Logf("OBJECTMETA, FIELD BY FIELD")
	row("  minus managedFields", nomfB, nomfO)
	row("  minus labels", nolabB, nolabO)
	row("  minus annotations", noannB, noannO)
	row("  minus ownerReferences", noownB, noownO)
	row("  minus all of the above", bareB, bareO)
	t.Logf("so: managedFields costs %.1f objs/pod, labels %.1f, annotations %.1f, ownerRefs %.1f, irreducible meta %.1f",
		float64(wo-nomfO)/n, float64(wo-nolabO)/n, float64(wo-noannO)/n, float64(wo-noownO)/n, float64(bareO)/n)
	_ = corev1.Pod{}
	goruntime.KeepAlive(raws)
}
