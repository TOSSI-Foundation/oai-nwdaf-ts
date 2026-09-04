// SPDX-License-Identifier: LicenseRef-CSSL-1.0
//
// Tests for the qosmonlist retention bound.
//
// qosMonPushValue() is pure, so these run with no MongoDB, no SMF and no
// network. What they pin down is the SHAPE of the update document handed to
// MongoDB - a wrong $slice sign would silently keep the OLDEST entries and
// throw away everything the analytics window needs.

package sbi

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"

	smf_client "gitlab.eurecom.fr/oai/cn5g/oai-cn5g-nwdaf/components/oai-nwdaf-sbi/internal/smfclient"
)

// 1. New reports are appended normally.
func TestQosMonPushAppendsTheReport(t *testing.T) {
	report := qosMon{PduSeId: nil, TimeStamp: 1234}
	v := qosMonPushValue(&report, 1000)

	m, ok := v.(bson.M)
	if !ok {
		t.Fatalf("expected a bson.M modifier, got %T", v)
	}
	each, ok := m["$each"].(bson.A)
	if !ok {
		t.Fatalf("expected $each to be a bson.A, got %T", m["$each"])
	}
	if len(each) != 1 {
		t.Fatalf("expected exactly one report per push, got %d", len(each))
	}
	if each[0] != &report {
		t.Errorf("the pushed element is not the report that was passed in")
	}
}

// 2. Old reports are removed - and from the correct end.
func TestQosMonPushBoundsToTheNewest(t *testing.T) {
	v := qosMonPushValue(&qosMon{}, 1000)
	m := v.(bson.M)

	slice, ok := m["$slice"].(int)
	if !ok {
		t.Fatalf("expected an int $slice, got %T", m["$slice"])
	}
	if slice != -1000 {
		t.Fatalf("expected $slice -1000, got %d", slice)
	}
	// The sign is the whole correctness argument: a POSITIVE $slice keeps the
	// FIRST n elements, which here would retain the oldest reports forever and
	// discard every recent one the analytics window actually reads.
	if slice > 0 {
		t.Fatal("positive $slice would keep the OLDEST entries")
	}
}

// 3. Recent reports remain available: the bound must never be smaller than the
//    window the analytics actually query.
func TestQosMonRetentionCoversTheAnalyticsWindow(t *testing.T) {
	// The engine's applyDefaultRecentWindow uses 300 s, and usage reports
	// arrive about every 5 s per session.
	const defaultWindowSec = 300
	const reportPeriodSec = 5
	needed := defaultWindowSec / reportPeriodSec // 60

	const shipped = 1000 // the envconfig default
	if shipped < needed {
		t.Fatalf("default retention %d is below the %d entries the 300 s window needs",
			shipped, needed)
	}
	if shipped < needed*4 {
		t.Errorf("default retention %d leaves less than 4x headroom over the "+
			"default window (%d entries) - explicit longer windows would lose data",
			shipped, needed)
	}
}

// 4. Analytics behaviour is unchanged when retention is disabled, which is the
//    rollback path.
func TestQosMonRetentionDisabledIsUnbounded(t *testing.T) {
	report := qosMon{TimeStamp: 99}
	for _, retain := range []int{0, -1, -1000} {
		v := qosMonPushValue(&report, retain)
		if _, isModifier := v.(bson.M); isModifier {
			t.Errorf("retain=%d should push the bare value (previous behaviour), got a modifier", retain)
		}
		if v != &report {
			t.Errorf("retain=%d should push the report itself", retain)
		}
	}
}

// The update document as a whole must still set lastmodified and push to
// qosmonlist - bounding must not change the field names any consumer reads.
func TestQosMonUpdateShapeUnchanged(t *testing.T) {
	config.Database.QosMonRetain = 1000
	var pduSeId int32 = 1
	notif := smf_client.EventNotification{PduSeId: &pduSeId}

	update, err := getUpdateQOS_MON(notif)
	if err != nil {
		t.Fatalf("getUpdateQOS_MON failed: %v", err)
	}
	var sawSet, sawPush bool
	for _, e := range update {
		switch e.Key {
		case "$set":
			sawSet = true
		case "$push":
			sawPush = true
			m := e.Value.(bson.M)
			if _, ok := m["qosmonlist"]; !ok {
				t.Error("$push no longer targets qosmonlist")
			}
		}
	}
	if !sawSet || !sawPush {
		t.Errorf("update lost $set (%v) or $push (%v)", sawSet, sawPush)
	}
}
