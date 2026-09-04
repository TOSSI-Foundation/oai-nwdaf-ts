/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * smf_nwdaf_consumer.hpp
 *
 * SMF as a 3GPP NWDAF analytics consumer.
 *
 * WHY THE SMF AND NOT THE PCF
 * TS 23.288 V17.12.0 Table 7.1-1 lists SMF as an example consumer of both
 * Nnwdaf_AnalyticsSubscription and Nnwdaf_AnalyticsInfo. For the specific
 * decision this project makes - which user-plane path a session should use -
 * the spec names the SMF, not the PCF:
 *
 *   clause 6.4.4: "The consumer SMF determines to (re)selects UP paths,
 *   including UPF and DNAI, as described in clause 4.3.5 of TS 23.502. In
 *   addition, the SMF may (re)configure traffic steering, updating the UPF
 *   regarding the target DNAI with new traffic steering rules."
 *
 *   clause 6.14 (DN Performance): "If the analytics consumer is an SMF, the
 *   SMF may use the analytics to determine the UPF and DNAI that offers the
 *   best user plane performance."
 *
 * The PCF's only UP-path role in TS 23.288 is clause 6.4.4's "The consumer PCF
 * may provide an updated list of DNAI(s) for SMF to perform relocation UPON AF
 * REQUEST" - conditional on an AF request, and this deployment has no AF. So
 * the PCF is not the specified consumer for a load-driven UPF decision.
 *
 * WHICH ANALYTICS ID
 * NF load information (TS 23.288 clause 6.5). Per clause 6.5.1 the consumer
 * indicates Analytics ID = "NF load information" and may supply Analytics
 * Filter Information containing "an optional list of NF Instance IDs, NF Set
 * IDs, or NF types" - which is how a specific UPF is targeted. The output is
 * defined by Table 6.5.3-1 (statistics) / Table 6.5.3-2 (predictions).
 *
 * This consumer deliberately does NOT request the project's custom
 * TRAFFIC_STEERING_UPF_LOAD Analytics ID. That ID is not defined in TS 23.288
 * Table 7.1-2 and an NF must not treat a vendor extension as a standard
 * analytic. NF_LOAD carries the same underlying UPF telemetry through a
 * standard Analytics ID.
 *
 * WHICH SERVICE OPERATION
 * Nnwdaf_AnalyticsInfo_Request (clause 6.1.2.1), i.e. request/response. Both
 * are permitted by clause 6.5.4 step 1 ("using either the Nnwdaf_AnalyticsInfo
 * or Nnwdaf_AnalyticsSubscription service"). Request/response is used here
 * because it is consumer-timed, needs no inbound callback endpoint on the SMF,
 * and is stateless across NWDAF restarts.
 *
 * SCOPE - READ THIS BEFORE ASSUMING THE LOOP IS CLOSED HERE
 * This class performs discovery, the analytics request, parsing and the
 * decision. It does NOT itself send PFCP. Enforcement remains the already
 * proven Route 2 chain (PCF -> SMF -> Update FAR/PDR -> UPF), which is
 * untouched. See docs/PROJECT-HISTORY.md for why: OAI declares
 * PDU_SESSION_MODIFICATION_SMF_REQUESTED in smf.h and handles it in
 * smf_procedure.cpp, but nothing anywhere constructs or triggers it, so the
 * SMF has no implemented self-initiated session-modification procedure to
 * hang an internally-generated decision on.
 */

#ifndef FILE_SMF_NWDAF_CONSUMER_HPP_SEEN
#define FILE_SMF_NWDAF_CONSUMER_HPP_SEEN

#include <atomic>
#include <map>
#include <memory>
#include <mutex>
#include <set>
#include <string>
#include <thread>
#include <utility>
#include <vector>

namespace oai::app::smf {
class smf_pdu_session;
}

namespace oai::smf {

/*
 * One NF-load reading for a single NF instance, carrying the values exactly as
 * the NWDAF reported them (TS 23.288 Table 6.5.3-1 / 6.5.3-2).
 *
 * confidence is present ONLY in predictions (Table 6.5.3-2); statistics
 * (Table 6.5.3-1) have no such field. has_confidence records whether the
 * NWDAF actually sent one, so that an absent confidence is never silently
 * turned into a value.
 */
struct nwdaf_nf_load_t {
  std::string nf_instance_id = {};
  int32_t nf_cpu_usage       = -1;
  int32_t nf_memory_usage    = -1;
  int32_t nf_load_level_average = -1;
  int32_t nf_load_level_peak    = -1;
  int32_t confidence            = -1;
  bool has_confidence           = false;

};

/*
 * One DN performance reading for a single path, carrying the values exactly as
 * the NWDAF reported them (TS 23.288 Table 6.14.3-1).
 *
 * Table 6.14.3-1 defines, per "DN performance" entry: "> Serving anchor UPF
 * info", "> DNAI" and the "> Performance Data" sub-fields. Only the two
 * traffic-rate sub-fields are represented here because they are the only ones
 * this deployment can measure - see docs/dn-performance-design.md section 4.
 * Average/Maximum Packet Delay and Average Packet Loss Rate are NOT modelled:
 * an unmeasurable field is absent, never zeroed.
 *
 * Table 6.14.3-1 has no Confidence field (that is Table 6.14.3-2, predictions),
 * so none is parsed - the same rule that kept it out of NF_LOAD statistics.
 *
 * The rates are stored in bit/s after parsing the TS 29.571 BitRate string.
 * has_* records whether the NWDAF actually sent a value, so an absent rate is
 * never silently read as 0 bit/s - which would rank a path with NO DATA as the
 * worst-performing path rather than as unknown.
 */
struct nwdaf_dn_perf_t {
  std::string dnai              = {};
  std::string upf_id            = {};
  std::string dnn               = {};
  double avg_traffic_rate_bps   = -1.0;
  bool has_avg_traffic_rate     = false;
  double max_traffic_rate_bps   = -1.0;
  bool has_max_traffic_rate     = false;
  // TS 23.288 Table 6.14.3-2 (PREDICTIONS) adds a Confidence to the fields of
  // Table 6.14.3-1. It is present ONLY in predictions; statistics have none, so
  // has_confidence records whether the NWDAF actually sent one rather than
  // letting an absent value become 0.
  int32_t confidence            = -1;
  bool has_confidence           = false;
  /*
   * PROJECT-SPECIFIC vendor extension `oaiPathHealthExt`, NOT a 3GPP field.
   *
   * Table 6.14.3-1's PerfData carries avgPacketLossRate / avePacketDelay /
   * maxPacketDelay. This is NONE of those: it is an AF_PACKET transmit-stall
   * indicator derived from VPP's `tx sendto temporary failure` counter,
   * normalised per transmitted packet. It is not packet loss - VPP `drops`,
   * Linux `tx_dropped` and `tx_errors` all read exactly zero under every
   * impairment measured. It is kept OUTSIDE the 3GPP fields for exactly that
   * reason, and is read here only by the HEALTH ranking rule.
   *
   * health_state is copied VERBATIM and never whitelisted, so a producer-side
   * state addition needs no change here. Known values:
   *   OBSERVED_HEALTHY  OBSERVED_DEGRADED
   *   UNKNOWN_NO_TRAFFIC  UNKNOWN_INSUFFICIENT_SAMPLES
   *   UNKNOWN_NO_SAMPLE   UNKNOWN_STALE
   *
   * An empty health_state means the producer sent no extension at all - which
   * is UNKNOWN, never healthy.
   *
   * has_health_ratio is false when the producer sent JSON null. The ratio MUST
   * NOT be read as 0 in that case: an idle path makes no transmit attempt, so
   * it cannot produce a failure, and an idle IMPAIRED path is byte-for-byte
   * identical to an idle healthy one (measured, PROJECT-HISTORY 28.6).
   */
  std::string health_state      = {};
  double health_ratio           = -1.0;
  bool has_health_ratio         = false;
  int64_t health_tx_attempts    = -1;
  int64_t health_age_sec        = -1;
  bool has_health_age           = false;
  // Absolute time the sample was taken. The serialization gate compares this
  // against the last steer, which an age alone cannot express.
  int64_t health_observed_at    = 0;
  bool has_health_observed_at   = false;
};

class smf_nwdaf_consumer {
 public:
  static smf_nwdaf_consumer& get_instance();

  smf_nwdaf_consumer(const smf_nwdaf_consumer&) = delete;
  void operator=(const smf_nwdaf_consumer&) = delete;

  /*
   * Read configuration from the environment and, if enabled, start the polling
   * thread. No-op unless SMF_NWDAF_ENABLE is "1"/"true"/"yes", so a deployment
   * that does not set it behaves exactly as before.
   */
  void start();

  /*
   * TS 23.288 clause 5.2: locate an NWDAF through the NRF.
   * Queries target-nf-type=NWDAF and builds the apiRoot of its
   * nnwdaf-analyticsinfo service.
   * @param [std::string&] api_root: discovered API root, e.g. http://ip:port
   * @return true if an NWDAF offering nnwdaf-analyticsinfo was found
   */
  bool discover_nwdaf(std::string& api_root);

  /*
   * TS 23.288 clause 6.2.2.4 / TS 23.502 clause 4.17.4: resolve the NF Instance
   * ID of the target UPF from the NRF instead of taking it from local
   * configuration.
   *
   * WHY THIS EXISTS. SMF_NWDAF_TARGET_UPF_NF_INSTANCE_ID used to be REQUIRED,
   * and the value deployed ("4f4f850a-...") matched no UPF registered in the
   * NRF - the NWDAF was configured with the same fabricated constant, so the
   * clause 6.5.1 filter check compared one invented value against another and
   * passed for the wrong reason. It also went stale on every UPF restart,
   * because the OAI UPF generates a fresh nfInstanceId each time.
   *
   * The selection rule deliberately mirrors process_upf_profile(): take the
   * FIRST UPF profile the NRF returns, so this names the UPF the SMF is
   * actually PFCP-associated with.
   *
   * @param [std::string&] nf_instance_id: discovered NF Instance ID
   * @return true if the NRF returned at least one UPF
   */
  bool discover_upf_nf_instance_id(std::string& nf_instance_id);

  /*
   * TS 23.288 clause 6.1.2.1 / 6.5.4 step 1:
   * Nnwdaf_AnalyticsInfo_Request with Analytics ID = NF load information.
   * @param [const std::string&] api_root: NWDAF apiRoot
   * @param [const std::string&] target_nf_instance_id: UPF NF Instance ID used
   *        as Analytics Filter Information (clause 6.5.1)
   * @param [std::vector<nwdaf_nf_load_t>&] out: parsed readings
   * @return true if the NWDAF answered with at least one NF load entry
   */
  bool request_nf_load(
      const std::string& api_root, const std::string& target_nf_instance_id,
      std::vector<nwdaf_nf_load_t>& out, bool& nwdaf_reachable);

  /*
   * TS 23.288 clause 6.1.2.1 / 6.14.4 step 1:
   * Nnwdaf_AnalyticsInfo_Request with Analytics ID = DN Performance.
   *
   * WHY THIS EXISTS ALONGSIDE request_nf_load(). NF_LOAD is a property of the
   * NF INSTANCE (Table 6.5.3-1). Both N6 DNAIs in this deployment are network
   * instances of the SAME UPF NF instance, so NF_LOAD structurally cannot rank
   * one against the other. DN_PERFORMANCE is the only standard analytic whose
   * output is per-path: Table 6.14.3-1 carries the serving anchor UPF info and
   * the DNAI per entry, and clause 6.14.4 names the SMF as the consumer -
   * "If the analytics consumer is an SMF, the SMF may use the analytics to
   * determine the UPF and DNAI that offers the best user plane performance."
   *
   * No Analytics Filter is sent in v1: the SMF wants every DNAI the NWDAF has
   * data for, precisely so it can compare them. Filtering to one DNAI would
   * defeat the purpose.
   *
   * @param [const std::string&] api_root: NWDAF apiRoot
   * @param [std::vector<nwdaf_dn_perf_t>&] out: parsed readings
   * @param [bool&] nwdaf_reachable: false only if the NWDAF could not be
   *        reached; an HTTP 204 sets it TRUE (well-formed "no data" answer)
   * @return true if the NWDAF answered with at least one usable DN perf entry
   */
  bool request_dn_performance(
      const std::string& api_root, std::vector<nwdaf_dn_perf_t>& out,
      bool& nwdaf_reachable);

  /*
   * The DNAI this consumer currently PREFERS based on NWDAF analytics, or an
   * empty string if no analytics have been consumed yet.
   *
   * THIS IS A PREFERENCE, NEVER AN AUTHORIZATION. It is derived from local
   * configuration (see SMF_NWDAF_DNAI_* in the .cpp) which TS 23.503 clause 6
   * permits as "a corresponding Traffic steering policy identifier (i.e. a
   * reference to a pre-configured traffic steering policy at the SMF)".
   * It must never be applied without first being checked against the DNAIs the
   * PCF authorized - use choose_authorized_dnai() for that.
   */
  std::string get_preferred_dnai();

  /*
   * Intersect the analytics-driven preference with the DNAIs the PCF has
   * authorized, and return a DNAI that is legal to steer to.
   *
   * The PCF is the policy authority (TS 23.503: "The PCF provides DNAI(s) in
   * the PCC rule(s) to the SMF"). The SMF may only (re)select a UP path
   * WITHIN that set (TS 23.288 clause 6.4.4).
   *
   * @param [const std::set<std::string>&] authorized: DNAIs the PCF authorized
   *        for this session, from get_authorized_dnais()
   * @param [std::string&] reason: human-readable explanation, always set
   * @return the DNAI to steer to, or an EMPTY string meaning "keep the current
   *         path". An empty return is a normal outcome, not an error.
   */
  std::string choose_authorized_dnai(
      const std::set<std::string>& authorized, std::string& reason);

 private:
  smf_nwdaf_consumer() = default;
  void poll_loop();
  bool decide(const nwdaf_nf_load_t& load, std::string& dnai,
              std::string& reason);

  /*
   * Rank the DNAIs the NWDAF reported and pick the best-performing one.
   *
   * 🔴 READ THIS BEFORE TRUSTING THE RESULT. With one UPF and one active path,
   * DN_PERFORMANCE can only measure the DNAI a session is ACTUALLY ON. The
   * rates for two DNAIs therefore come from DIFFERENT time windows that
   * carried DIFFERENT offered load. This is a MEASUREMENT and a feedback
   * signal ("did the last steer help?"), NOT a counterfactual and NOT a
   * prediction of how the unused path would perform. A true counterfactual
   * needs concurrently active paths, i.e. multi-UPF - a topology change, not
   * code. See docs/dn-performance-design.md section 4.1.
   *
   * CLASSIFICATION: the ordering rule below is PROJECT-SPECIFIC decision
   * logic. TS 23.288 clause 6.14.1 defines dnPerfOrderCriter / order /
   * reportThresholds for the CONSUMER to express an ordering preference to the
   * NWDAF; evaluating those is explicitly deferred (design section 10). What
   * this method does is choose locally among the entries returned.
   *
   * @param [const std::vector<nwdaf_dn_perf_t>&] perfs: parsed readings
   * @param [std::string&] dnai: the DNAI preferred, set only when true
   * @param [std::string&] reason: human-readable explanation, always set
   * @return true if a preference could be formed
   */
  bool decide_from_dn_performance(
      const std::vector<nwdaf_dn_perf_t>& perfs, std::string& dnai,
      std::string& reason);

  /*
   * The HEALTH ranking rule (SMF_NWDAF_DNPERF_RULE=HEALTH).
   *
   * WHY THIS IS PER-SESSION AND decide_from_dn_performance() IS NOT.
   * The RATE rule ranks paths globally and compares against the SMF's own
   * previous preference. This rule asks a different question - "is the path
   * THIS session is on actually degraded?" - and "the path this session is on"
   * is a property of the session (get_n6_confirmed_dnai()), not of the SMF.
   * With two DNAIs a global answer happens to come out right; with three it
   * does not, so the question is asked where the answer is actually known.
   *
   * THE RULE:
   *   current OBSERVED_DEGRADED -> steer to the best authorized alternative
   *   current OBSERVED_HEALTHY  -> HOLD
   *   current UNKNOWN_*         -> HOLD   (unknown is NOT bad)
   *   current health absent     -> HOLD
   *
   * Leaving a KNOWN-BAD path for an unknown one is defensible. Leaving an
   * unknown path is not - that would be acting on no information at all.
   *
   * Candidate selection among authorized DNAIs other than the current one:
   *   1. exclude OBSERVED_DEGRADED
   *   2. exclude "suspect" - degraded within m_degraded_memory_sec and not
   *      seen OBSERVED_HEALTHY since. This is what stops the A->B->A ping-pong
   *      that a naive health rule would produce: steer off a degraded path and
   *      it goes idle, whereupon it reads UNKNOWN_NO_TRAFFIC - indistinguishable
   *      from healthy - and a rule without memory would steer straight back.
   *   3. prefer OBSERVED_HEALTHY over UNKNOWN_*
   *   4. within a tier, lowest avgTrafficRate (least loaded) wins. The
   *      confidence floor still applies HERE, because this is the only place a
   *      rate is used. A DNAI with no usable rate sorts LAST in its tier:
   *      unknown load is not zero load.
   *
   * @param [const std::string&] current_dnai: the UPF-CONFIRMED DNAI of this
   *        session - intent is not truth, see PROJECT-HISTORY 19.7
   * @param [const std::set<std::string>&] authorized: PCF-authorized DNAIs
   * @param [std::string&] dnai: the DNAI to steer to, set only when true
   * @param [std::string&] reason: human-readable explanation, always set
   * @return true if a steer is warranted
   */
  bool decide_for_session(
      const std::string& current_dnai,
      const std::set<std::string>& authorized, std::string& dnai,
      std::string& reason);

  /*
   * Record which DNAIs were seen OBSERVED_DEGRADED, and forget it again.
   *
   * An OBSERVED_HEALTHY reading CLEARS the entry immediately - that is direct
   * evidence of recovery. An UNKNOWN_* reading never clears it, which is the
   * whole point: an idle path reads UNKNOWN whether it is healthy or broken.
   * Entries also expire after m_degraded_memory_sec, so a path that has been
   * left alone long enough becomes a candidate again. That is the decay.
   */
  void update_degraded_memory(const std::vector<nwdaf_dn_perf_t>& perfs);

  /*
   * True if this reading's path health is recent enough to act on. The
   * producer already suppresses samples older than its own bound and reports
   * UNKNOWN_STALE; this is the consumer applying its own, independent of what
   * the producer was configured with. A sample of unknown age is never used.
   */
  bool health_is_fresh(const nwdaf_dn_perf_t& p) const;

  /*
   * One DN_PERFORMANCE poll iteration. Kept separate from poll_loop()'s
   * NF_LOAD body so that the proven NF_LOAD path is not restructured.
   */
  void poll_dn_performance(const std::string& api_root, bool& nwdaf_reachable);
  /*
   * Resolve, per PDU session, what the analytics preference legally amounts to
   * given that session's own PCF authorization - and, if actuation is enabled,
   * ask the SMF-APP task to apply it.
   */
  void evaluate_sessions();

  /*
   * Ask TASK_SMF_APP to apply an SMF-INITIATED traffic-steering
   * reconfiguration for one PDU session.
   *
   * THREADING - THIS IS THE WHOLE POINT OF THE FUNCTION. It runs on this
   * consumer's own detached thread. It therefore does NOT touch the session
   * graph, does NOT run a procedure and does NOT send PFCP; it only posts an
   * ITTI message. Everything else happens on TASK_SMF_APP, exactly as it does
   * for the PCF-initiated path. Running the procedure here would make this
   * thread a second writer to smf_context's procedure state.
   *
   * The DNAI has ALREADY passed choose_authorized_dnai() when this is called.
   * It is re-checked against the session's CURRENT policy on the SMF-APP side
   * before anything is applied, because the PCF may revoke in between.
   *
   * No-op unless SMF_NWDAF_ACT is enabled, and no-op when the session is
   * already on the chosen DNAI.
   *
   * @param [smf_pdu_session] sp: the session to steer
   * @param [std::string] chosen: the PCF-authorized DNAI to apply
   * @param [int] pdu_session_id: for logging only
   * @return true only if a steering request was actually SENT. False for every
   *         no-op path (actuation disabled, already on the target DNAI, still
   *         inside the in-flight quiet period, ITTI send failed). The caller
   *         uses this to decide whether the cycle's steering budget was spent -
   *         counting a no-op would let one declined session block a real move
   *         for a whole cycle.
   */
  bool maybe_trigger_steering(
      const std::shared_ptr<oai::app::smf::smf_pdu_session>& sp,
      const std::string& chosen, int pdu_session_id);

  std::thread m_thread          = {};
  std::atomic<bool> m_running   = {false};
  std::mutex m_mutex            = {};
  std::string m_preferred_dnai   = {};
  std::string m_cached_api_root  = {};
  // NF Instance ID of the UPF this consumer asks about. Resolved from the NRF
  // unless SMF_NWDAF_TARGET_UPF_NF_INSTANCE_ID pins it explicitly.
  std::string m_target_upf_id_resolved = {};
  bool m_target_upf_id_is_static       = false;

  // configuration (environment-sourced, see .cpp)
  bool m_enabled              = false;
  std::string m_static_uri    = {};
  std::string m_target_upf_id = {};
  std::string m_dnai_normal    = {};
  std::string m_dnai_congested = {};
  int m_poll_sec               = 10;
  int m_load_threshold         = 0;
  int m_clear_streak_required  = 2;
  int m_clear_streak           = 0;

  // Which Analytics ID this consumer requests: "NF_LOAD" (default, the proven
  // path) or "DN_PERFORMANCE". Both are TS 23.288 Table 7.1-2 Analytics IDs.
  // Unset behaves exactly as before this feature existed.
  std::string m_analytics_id = "NF_LOAD";
  // PROJECT-SPECIFIC, not 3GPP: minimum relative advantage the best DNAI must
  // show over the currently preferred one before the preference moves, to stop
  // it flapping on measurement noise between two windows.
  int m_dnperf_margin_percent = 10;

  // ACTUATION. Default FALSE: with SMF_NWDAF_ACT unset the consumer behaves
  // exactly as it did before - it resolves the per-session decision and only
  // LOGS it, and nothing this class does can move a PDU session.
  bool m_act_enabled = false;
  // PREDICTION. TS 29.520 EventReportingRequirement.offsetPeriod: "a positive
  // value means prediction in the future offset period". 0 (default) requests
  // STATISTICS, which is exactly how this consumer behaved before.
  int m_predict_sec = 0;
  // Predictions below this confidence are not acted on. TS 23.288 defines no
  // threshold - this is PROJECT-SPECIFIC. Ignored entirely for statistics,
  // which carry no confidence and must not be judged as if they did.
  int m_min_confidence = 50;
  // Re-trigger damping. The poll runs every m_poll_sec, and applying a steer
  // is not instantaneous, so without this the same modification would be
  // re-issued on the next tick before the first one had taken effect.
  // scid -> (dnai last asked for, monotonic seconds when it was asked).
  std::map<uint64_t, std::pair<std::string, int64_t>> m_last_trigger = {};

  // Which ranking rule decides. "RATE" (default) is the original, unchanged
  // argmax(avgTrafficRate) behaviour; "HEALTH" gates on the per-DNAI path
  // health extension. Default RATE so deploying this build changes NOTHING
  // until the operator opts in, and rollback is one environment variable with
  // no rebuild.
  std::string m_dnperf_rule = "RATE";
  // Consumer-side freshness bound on the health extension, independent of the
  // producer's own. Anything older is treated as UNKNOWN.
  int m_health_max_age_sec = 30;
  // How long a DNAI stays "suspect" after being seen OBSERVED_DEGRADED, unless
  // an OBSERVED_HEALTHY reading clears it sooner. Stops the ping-pong.
  int m_degraded_memory_sec = 120;
  // Minimum interval between two steers of the SAME session under the HEALTH
  // rule. Broader than m_last_trigger, which only suppresses a repeat of the
  // SAME target and so does nothing against A->B->A oscillation.
  int m_steer_cooldown_sec = 60;

  // GLOBAL STEERING SERIALIZATION (PROJECT-HISTORY 31.12, Improvement 6b).
  // At most m_max_steers_per_cycle sessions may move per evaluate_sessions()
  // call, and the poll loop calls that exactly once per poll - so the limit is
  // global to the steering engine, not per session. The existing per-session
  // cooldown and in-flight quiet period are UNCHANGED and still apply on top.
  int m_max_steers_per_cycle = 1;
  // Unix seconds of the last steer issued for ANY session. The next cycle will
  // not move another session until it has a health observation taken after
  // this instant - see smf_nwdaf_steer_serialization.hpp.
  int64_t m_last_global_steer_at = 0;

  // dnai -> unix seconds when it was last seen OBSERVED_DEGRADED.
  std::map<std::string, int64_t> m_dnai_degraded_at = {};
  // scid -> unix seconds of the last steer issued for that session.
  std::map<uint64_t, int64_t> m_last_steer_at = {};
  // The most recent parsed readings, so the per-session evaluation can consult
  // them without re-issuing the analytics request once per PDU session.
  std::vector<nwdaf_dn_perf_t> m_last_perfs = {};
  // When those readings were FETCHED. Without this the consumer keeps steering
  // on the last successful poll forever whenever the NWDAF becomes unreachable:
  // health_is_fresh() tests `ageSec`, which is a snapshot taken at fetch time
  // and never grows, so a stale cache passes the freshness test indefinitely.
  // Observed live - the analytics endpoint slowed to 3-4.5 s as `upf_metrics`
  // grew past 41 000 documents, the SMF's requests timed out ("NWDAF returned
  // HTTP 0"), and decisions carried on against readings that predated an
  // impairment applied minutes earlier.
  int64_t m_last_perfs_at = 0;
};

}  // namespace oai::smf

#endif
