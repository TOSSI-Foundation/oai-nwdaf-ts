// SPDX-License-Identifier: LicenseRef-CSSL-1.0
//
// Unit tests for the global steering serialization gate.
//
// The gate is a pure predicate, so it is tested directly with no SMF runtime:
// no smf_app_inst, no ITTI, no HTTP client, no MongoDB.
//
//   g++ -std=c++17 -I<smf_app> tests/test_steer_serialization.cpp -o /tmp/t && /tmp/t

#include "smf_nwdaf_steer_serialization.hpp"

#include <cstdio>
#include <string>
#include <vector>

using oai::smf::serialization_allows_steer;
using oai::smf::steer_cycle_state;

static int g_fail = 0;
static void check(bool cond, const std::string& what) {
  std::printf("  [%s] %s\n", cond ? "PASS" : "FAIL", what.c_str());
  if (!cond) ++g_fail;
}

// Drive one evaluation cycle over `n` sessions that are ALL eligible and all
// targeting the same DNAI, exactly as a herd would. Returns how many were
// allowed to move. Mirrors evaluate_sessions(): one budget, decremented only
// by an action that actually happened.
static int cycle(int n, steer_cycle_state& st, bool target_has_health,
                 int64_t observed_at, int64_t steer_clock) {
  int moved = 0;
  for (int i = 0; i < n; ++i) {
    std::string why;
    if (serialization_allows_steer(st, target_has_health, observed_at, why)) {
      ++st.steers_this_cycle;
      st.last_global_steer_at = steer_clock;
      ++moved;
    }
  }
  return moved;
}

int main() {
  // ---- Case 1: herd prevention ------------------------------------------
  // 4 sessions on a degraded path, all eligible, all targeting the healthy one.
  std::printf("Case 1 - herd prevention (4 eligible sessions)\n");
  {
    steer_cycle_state st = {};
    st.max_per_cycle = 1;
    int t = 1000;
    // Poll 1: nothing has ever been steered, health is irrelevant.
    int m1 = cycle(4, st, true, t - 1, t);
    check(m1 == 1, "poll 1 moves exactly 1 of 4 (was: all 4 at once)");

    // Poll 2: fresh cycle, and a health observation taken AFTER the poll-1 move.
    steer_cycle_state st2 = {};
    st2.max_per_cycle = 1;
    st2.last_global_steer_at = t;
    int m2 = cycle(3, st2, true, t + 5, t + 10);
    check(m2 == 1, "poll 2 moves at most 1 more");

    steer_cycle_state st3 = {};
    st3.max_per_cycle = 1;
    st3.last_global_steer_at = t + 10;
    int m3 = cycle(2, st3, true, t + 15, t + 20);
    check(m3 == 1, "poll 3 moves at most 1 more");
    check(m1 + m2 + m3 == 3, "3 polls move 3 sessions, never 4 in one cycle");
  }

  // ---- Case 2: target degraded, and the stale-health guard ---------------
  // The "target became OBSERVED_DEGRADED" exclusion lives in
  // decide_for_session()'s candidate filter, so by the time the gate is reached
  // `chosen` is already empty and no session can migrate. What the gate adds is
  // the case BEFORE that is visible: a health sample older than the last steer
  // cannot show its effect, so no second session may ride on it.
  std::printf("Case 2 - target health must postdate the last steer\n");
  {
    steer_cycle_state st = {};
    st.max_per_cycle = 1;
    st.last_global_steer_at = 2000;
    std::string why;
    check(!serialization_allows_steer(st, true, 1995, why),
          "health observed BEFORE the last steer is refused");
    check(why.find("waiting for an observation") != std::string::npos,
          "refusal explains it is waiting for a fresh observation");
    check(serialization_allows_steer(st, true, 2001, why),
          "health observed AFTER the last steer is accepted");
    // A target with no health at all (the RATE rule) gets the budget only.
    check(serialization_allows_steer(st, false, 0, why),
          "no health on the target -> budget only, no freshness gate");
  }

  // ---- Case 3: interaction with the existing per-session cooldown --------
  // The cooldown lives in maybe_trigger_steering() and is unchanged. The gate
  // must not consume budget for a session the cooldown then declines - which is
  // why evaluate_sessions() increments only on a TRUE return. Simulated here.
  std::printf("Case 3 - a declined action must not spend the budget\n");
  {
    steer_cycle_state st = {};
    st.max_per_cycle = 1;
    std::string why;
    check(serialization_allows_steer(st, false, 0, why),
          "gate permits the first session");
    // The per-session cooldown declines it: budget NOT incremented.
    check(serialization_allows_steer(st, false, 0, why),
          "a second session may still move after a declined no-op");
    st.steers_this_cycle = 1;  // now a real steer happened
    check(!serialization_allows_steer(st, false, 0, why),
          "after a REAL steer the budget is spent");
  }

  // ---- Case 4: nothing eligible / limit disabled -------------------------
  std::printf("Case 4 - no eligible sessions, and the disable switch\n");
  {
    steer_cycle_state st = {};
    st.max_per_cycle = 1;
    check(cycle(0, st, true, 1, 1) == 0, "0 eligible sessions -> 0 steers");
    check(st.steers_this_cycle == 0, "budget untouched when nothing is eligible");

    steer_cycle_state off = {};
    off.max_per_cycle = 0;  // disabled -> previous all-at-once behaviour
    check(cycle(4, off, false, 0, 1) == 4,
          "max_per_cycle<=0 restores all-at-once (rollback path)");
  }

  // ---- invariant ---------------------------------------------------------
  std::printf("Invariant - at most one steering action per cycle\n");
  {
    bool ok = true;
    for (int n = 1; n <= 50; ++n) {
      steer_cycle_state st = {};
      st.max_per_cycle = 1;
      if (cycle(n, st, true, 10, 20) > 1) ok = false;
    }
    check(ok, "for 1..50 eligible sessions, one cycle never exceeds 1 steer");
  }

  std::printf("\n%s\n", g_fail == 0 ? "ALL TESTS PASSED" : "FAILURES PRESENT");
  return g_fail == 0 ? 0 : 1;
}
