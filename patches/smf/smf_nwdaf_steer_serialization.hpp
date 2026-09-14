/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 *
 * smf_nwdaf_steer_serialization.hpp
 *
 * The global serialization gate for NWDAF-driven UP path steering.
 *
 * WHY THIS EXISTS.
 * decide_for_session() is evaluated PER SESSION, but the path-health signal it
 * consumes is per DNAI. Every session on a degraded path therefore reaches the
 * same verdict in the same polling cycle and they all migrate together - a
 * thundering herd. With 5 UEs, four sessions moved inside one 10 s poll. The
 * per-session cooldown cannot prevent this,
 * because it is per session and they all fire at once.
 *
 * WHY IT IS A SEPARATE HEADER.
 * The rule is a pure predicate over three numbers, so keeping it out of the
 * consumer makes it unit-testable without standing up smf_app_inst, itti_inst
 * or an HTTP client. Nothing here touches SMF state.
 *
 * WHAT IT DOES NOT DO.
 * No capacity model, no throughput ranking, no jitter, no prediction. It only
 * answers "may one more session move right now?".
 */
#ifndef FILE_SMF_NWDAF_STEER_SERIALIZATION_HPP_SEEN
#define FILE_SMF_NWDAF_STEER_SERIALIZATION_HPP_SEEN

#include <cstdint>
#include <string>

namespace oai::smf {

/*
 * The steering budget for ONE evaluation cycle. Reset at the top of every
 * evaluate_sessions() call, which the poll loop invokes exactly once per poll -
 * that is what makes the limit global to the steering engine rather than
 * per session.
 */
struct steer_cycle_state {
  // How many sessions have actually been steered so far in THIS cycle. Counted
  // on the ITTI message actually being sent, not on the decision being taken:
  // a decision that maybe_trigger_steering() then declines (already on the
  // target DNAI, or still inside the in-flight quiet period) must not consume
  // the budget, or one no-op would block a real move for a whole cycle.
  int steers_this_cycle = 0;
  // Maximum steering actions per cycle. 1 = fully serialized. <= 0 disables the
  // limit entirely and restores the previous all-at-once behaviour.
  int max_per_cycle = 1;
  // Unix seconds of the last steer issued by this consumer, across ALL
  // sessions and cycles. 0 = none yet.
  int64_t last_global_steer_at = 0;
};

/*
 * May one more session be steered right now?
 *
 * Two independent gates:
 *
 *  1. BUDGET - at most max_per_cycle actions per evaluation cycle.
 *
 *  2. FRESHNESS - after a steer, the next cycle must decide on a health
 *     observation that POSTDATES that steer. Without this the budget alone
 *     would still herd: the SMF polls every 10 s while the collector samples
 *     every ~5 s and the consumer accepts samples up to 30 s old, so cycle N+1
 *     can easily read a sample taken BEFORE the cycle-N move and therefore
 *     showing none of its effect. Sessions would then trickle onto a target
 *     that is already saturating, one per cycle, still blind. Requiring
 *     observedAt > last_global_steer_at is what makes this closed-loop rather
 *     than merely rate-limited.
 *
 * The freshness gate applies only when the target actually carries health
 * (target_has_health). The RATE rule has none, so it gets the budget only -
 * which is the intended minimal change to that path.
 *
 * @param [const steer_cycle_state&] st: the current cycle's budget and history
 * @param [bool] target_has_health: whether the target DNAI reported path health
 * @param [int64_t] target_health_observed_at: unix seconds the target's health
 *        sample was taken (meaningful only when target_has_health)
 * @param [std::string&] reason: set whenever the answer is false
 * @return true if a steering action may proceed
 */
inline bool serialization_allows_steer(
    const steer_cycle_state& st, bool target_has_health,
    int64_t target_health_observed_at, std::string& reason) {
  if (st.max_per_cycle > 0 && st.steers_this_cycle >= st.max_per_cycle) {
    reason = "steering cycle budget spent (" +
             std::to_string(st.steers_this_cycle) + "/" +
             std::to_string(st.max_per_cycle) +
             ") - holding remaining sessions until the next cycle";
    return false;
  }
  if (st.last_global_steer_at > 0 && target_has_health &&
      target_health_observed_at <= st.last_global_steer_at) {
    reason = "target health was observed at " +
             std::to_string(target_health_observed_at) +
             ", not after the last steer at " +
             std::to_string(st.last_global_steer_at) +
             " - waiting for an observation that reflects it";
    return false;
  }
  return true;
}

}  // namespace oai::smf

#endif
