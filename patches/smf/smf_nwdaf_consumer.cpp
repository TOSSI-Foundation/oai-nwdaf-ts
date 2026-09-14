/*
 * SPDX-License-Identifier: LicenseRef-CSSL-1.0
 */

/*
 * smf_nwdaf_consumer.cpp - see smf_nwdaf_consumer.hpp for the TS 23.288
 * clause-by-clause justification of why the SMF is the consumer, why the
 * Analytics ID is NF load information, and what this class deliberately does
 * NOT do.
 */

#include "smf_nwdaf_consumer.hpp"

#include "smf_nwdaf_steer_serialization.hpp"

#include <cctype>
#include <cstdio>
#include <cstdlib>
#include <ctime>
#include <nlohmann/json.hpp>

#include "http_client.hpp"
#include "itti.hpp"
#include "smf_app.hpp"
#include "smf_pfcp_association.hpp"
#include "logger.hpp"
#include "sbi_helper.hpp"
#include "smf_config.hpp"
#include "smf_sbi_helper.hpp"

using namespace oai::common::sbi;
using namespace oai::http;
using json = nlohmann::json;

extern std::unique_ptr<oai::config::smf::smf_config> smf_cfg;
extern std::shared_ptr<oai::http::http_client> http_client_inst;
extern oai::app::smf::smf_app* smf_app_inst;
extern itti_mw* itti_inst;

namespace oai::smf {

//------------------------------------------------------------------------------
static std::string env_str(const char* name, const std::string& fallback) {
  const char* v = std::getenv(name);
  return (v && *v) ? std::string(v) : fallback;
}

static int env_int(const char* name, int fallback) {
  const char* v = std::getenv(name);
  if (!v || !*v) return fallback;
  try {
    return std::stoi(v);
  } catch (const std::exception&) {
    return fallback;
  }
}

static bool env_bool(const char* name) {
  const std::string v = env_str(name, "");
  return v == "1" || v == "true" || v == "TRUE" || v == "yes" || v == "YES";
}

//------------------------------------------------------------------------------
// TS 29.571 BitRate: a STRING, not a number - "String representing a bit rate,
// prefixes follow the standard symbols from The International System of Units,
// and represent x1000 multipliers, with the exception that prefix 'K' is used
// to represent the standard symbol 'k'." The value is a decimal, a single
// space, then the unit: e.g. "445.5 Mbps".
//
// Returns false if the string is absent or does not parse. The caller must
// then treat the rate as ABSENT, never as 0 bit/s: a path with no data ranked
// as 0 would look like the worst-performing path instead of an unknown one.
static bool parse_bitrate_bps(const std::string& value, double& bps) {
  if (value.empty()) return false;

  const std::size_t space = value.find(' ');
  if (space == std::string::npos || space == 0) return false;

  double number = 0.0;
  try {
    std::size_t consumed = 0;
    number               = std::stod(value.substr(0, space), &consumed);
    if (consumed != space) return false;
  } catch (const std::exception&) {
    return false;
  }
  if (number < 0.0) return false;

  const std::string unit = value.substr(space + 1);
  double multiplier      = 0.0;
  if (unit == "bps") {
    multiplier = 1.0;
  } else if (unit == "Kbps") {
    multiplier = 1e3;
  } else if (unit == "Mbps") {
    multiplier = 1e6;
  } else if (unit == "Gbps") {
    multiplier = 1e9;
  } else if (unit == "Tbps") {
    multiplier = 1e12;
  } else {
    // Anything else is not a TS 29.571 BitRate. Do not guess a multiplier.
    return false;
  }

  bps = number * multiplier;
  return true;
}

//------------------------------------------------------------------------------
// Inverse of the above, for logging only - never for anything put on the wire.
static std::string bitrate_to_string(double bps) {
  char buf[64] = {};
  if (bps >= 1e9) {
    std::snprintf(buf, sizeof(buf), "%.2f Gbps", bps / 1e9);
  } else if (bps >= 1e6) {
    std::snprintf(buf, sizeof(buf), "%.2f Mbps", bps / 1e6);
  } else if (bps >= 1e3) {
    std::snprintf(buf, sizeof(buf), "%.2f Kbps", bps / 1e3);
  } else {
    std::snprintf(buf, sizeof(buf), "%.2f bps", bps);
  }
  return std::string(buf);
}

//------------------------------------------------------------------------------
smf_nwdaf_consumer& smf_nwdaf_consumer::get_instance() {
  static smf_nwdaf_consumer instance;
  return instance;
}

//------------------------------------------------------------------------------
/*
 * Configuration is taken from the environment rather than from the SMF YAML
 * schema on purpose: the YAML config is validated against a fixed schema in
 * smf_config_types.cpp and adding keys there would touch the parsing path that
 * every existing deployment depends on. Environment variables keep the blast
 * radius of this feature to files that did not exist before, and the feature
 * is off unless SMF_NWDAF_ENABLE is set. This is a deliberate minimal-footprint
 * choice.
 */
void smf_nwdaf_consumer::start() {
  m_enabled = env_bool("SMF_NWDAF_ENABLE");
  if (!m_enabled) {
    Logger::smf_app().debug(
        "NWDAF consumer disabled (set SMF_NWDAF_ENABLE=1 to enable)");
    return;
  }
  m_static_uri    = env_str("SMF_NWDAF_URI", "");
  // OPTIONAL OVERRIDE ONLY. Left unset, the target UPF's NF Instance ID is
  // resolved from the NRF (TS 23.288 clause 6.2.2.4), which is the only source
  // that stays correct across a UPF restart.
  m_target_upf_id = env_str("SMF_NWDAF_TARGET_UPF_NF_INSTANCE_ID", "");
  m_target_upf_id_is_static = !m_target_upf_id.empty();
  // PREFERENCE ORDERING ONLY - never the final answer. TS 23.503 clause 6
  // allows "a pre-configured traffic steering policy at the SMF", but the DNAI
  // actually applied must come from choose_authorized_dnai(), which intersects
  // this preference with what the PCF authorized for that specific session.
  m_dnai_normal   = env_str("SMF_NWDAF_DNAI_NORMAL", "internet-primary");
  m_dnai_congested =
      env_str("SMF_NWDAF_DNAI_CONGESTED", "internet-secondary");
  m_poll_sec              = env_int("SMF_NWDAF_POLL_SEC", 10);
  m_load_threshold        = env_int("SMF_NWDAF_LOAD_THRESHOLD", 50);
  m_clear_streak_required = env_int("SMF_NWDAF_CLEAR_STREAK", 2);

  // Which TS 23.288 Table 7.1-2 Analytics ID to request. Defaults to NF_LOAD,
  // so an existing deployment that does not set this behaves exactly as
  // before. Any unrecognised value falls back to NF_LOAD with a warning rather
  // than silently requesting something the NWDAF does not serve.
  m_analytics_id = env_str("SMF_NWDAF_ANALYTICS_ID", "NF_LOAD");
  if (m_analytics_id != "NF_LOAD" && m_analytics_id != "DN_PERFORMANCE") {
    Logger::smf_app().warn(
        "NWDAF consumer: SMF_NWDAF_ANALYTICS_ID='%s' is not an Analytics ID "
        "this consumer implements (NF_LOAD | DN_PERFORMANCE) - falling back to "
        "NF_LOAD",
        m_analytics_id.c_str());
    m_analytics_id = "NF_LOAD";
  }
  m_dnperf_margin_percent = env_int("SMF_NWDAF_DNPERF_MARGIN_PERCENT", 10);

  // ACTUATION - default OFF. Unset, this consumer only ever LOGS its decision,
  // which is exactly how it behaved before the self-trigger existed.
  m_predict_sec    = env_int("SMF_NWDAF_PREDICT_SEC", 0);
  m_min_confidence = env_int("SMF_NWDAF_MIN_CONFIDENCE", 50);
  if (m_predict_sec > 0) {
    Logger::smf_app().info(
        "NWDAF consumer: requesting PREDICTIONS %ds ahead (TS 29.520 "
        "EventReportingRequirement.offsetPeriod), acting only on a confidence "
        ">= %d. TS 23.288 Table 6.14.3-2 adds Confidence to the statistics "
        "fields of Table 6.14.3-1; how it is computed is producer-specific and "
        "this NWDAF documents its own definition.",
        m_predict_sec, m_min_confidence);
  }

  // Which ranking rule decides. Default "RATE" = the original behaviour, so a
  // deployment that does not set this is bit-for-bit unchanged.
  m_dnperf_rule = env_str("SMF_NWDAF_DNPERF_RULE", "RATE");
  for (auto& c : m_dnperf_rule) c = std::toupper(c);
  if (m_dnperf_rule != "RATE" && m_dnperf_rule != "HEALTH") {
    Logger::smf_app().warn(
        "SMF_NWDAF_DNPERF_RULE='%s' is not RATE or HEALTH - falling back to "
        "RATE",
        m_dnperf_rule.c_str());
    m_dnperf_rule = "RATE";
  }
  // Global serialization. 1 = one session may move per evaluation cycle, which
  // is what stops a path-scoped signal from migrating every session at once.
  // Set <= 0 to restore the previous all-at-once behaviour.
  m_max_steers_per_cycle = env_int("SMF_NWDAF_STEER_MAX_PER_CYCLE", 1);
  m_health_max_age_sec  = env_int("SMF_NWDAF_HEALTH_MAX_AGE_SEC", 30);
  m_degraded_memory_sec = env_int("SMF_NWDAF_DEGRADED_MEMORY_SEC", 120);
  m_steer_cooldown_sec  = env_int("SMF_NWDAF_STEER_COOLDOWN_SEC", 60);
  if (m_dnperf_rule == "HEALTH") {
    Logger::smf_app().info(
        "NWDAF DN_PERFORMANCE ranking rule: HEALTH - steer only AWAY from a "
        "path observed degraded; hold on healthy and on every UNKNOWN state. "
        "PROJECT-SPECIFIC, not 3GPP. freshness=%ds degraded-memory=%ds "
        "cooldown=%ds max-steers-per-cycle=%d",
        m_health_max_age_sec, m_degraded_memory_sec, m_steer_cooldown_sec,
        m_max_steers_per_cycle);
  }

  m_act_enabled = env_bool("SMF_NWDAF_ACT");
  if (m_act_enabled) {
    Logger::smf_app().warn(
        "NWDAF consumer: SMF_NWDAF_ACT is enabled - the SMF will APPLY its own "
        "UP path decisions (SMF-initiated traffic-steering reconfiguration), "
        "with no AF and no PCF notification involved. Every applied DNAI is "
        "still re-checked against that session's PCF authorization at "
        "execution time; the PCF remains the policy authority (TS 23.503).");
  }

  if (m_target_upf_id_is_static) {
    Logger::smf_app().warn(
        "NWDAF consumer: SMF_NWDAF_TARGET_UPF_NF_INSTANCE_ID=%s pins the "
        "target UPF to a STATIC NF Instance ID. The OAI UPF generates a new "
        "nfInstanceId on every restart, so this WILL go stale. Unset it to "
        "resolve the identity from the NRF (TS 23.288 clause 6.2.2.4).",
        m_target_upf_id.c_str());
    m_target_upf_id_resolved = m_target_upf_id;
  }

  if (m_analytics_id == "DN_PERFORMANCE") {
    Logger::smf_app().info(
        "NWDAF consumer starting: Analytics ID=DN_PERFORMANCE (TS 23.288 "
        "clause 6.14), poll=%ds, switch margin=%d%%. The DNAI preference is "
        "ranked from measured avgTrafficRate per DNAI; SMF_NWDAF_DNAI_NORMAL / "
        "_CONGESTED and SMF_NWDAF_LOAD_THRESHOLD are NF_LOAD settings and are "
        "not used on this path.",
        m_poll_sec, m_dnperf_margin_percent);
  } else {
    Logger::smf_app().info(
        "NWDAF consumer starting: Analytics ID=NF_LOAD, target UPF NF Instance "
        "ID=%s, poll=%ds, load threshold=%d, DNAI normal=%s congested=%s",
        m_target_upf_id_is_static ? m_target_upf_id.c_str()
                                  : "<to be resolved from the NRF>",
        m_poll_sec, m_load_threshold,
        m_dnai_normal.c_str(), m_dnai_congested.c_str());
  }

  m_running = true;
  m_thread  = std::thread(&smf_nwdaf_consumer::poll_loop, this);
  m_thread.detach();
}

//------------------------------------------------------------------------------
// TS 23.288 clause 5.2 - NWDAF Discovery and Selection.
//
// NF load analytics are "NF related" in the sense of clause 5.2 ("NF related
// refers to analytics/data that do not require a SUPI nor group of SUPIs (e.g.
// NF load analytics)"), so no UE identity is involved in the discovery and the
// consumer may simply take a candidate from the NRF response. This deployment
// runs a single NWDAF, so the "select an NWDAF with large serving area"
// preference in clause 5.2 has no candidates to choose between; the first
// instance offering nnwdaf-analyticsinfo is used.
bool smf_nwdaf_consumer::discover_nwdaf(std::string& api_root) {
  std::string nrf_uri =
      oai::smf::api::smf_sbi_helper::get_nrf_disc_search_nf_instances_uri(
          smf_cfg->get_nf(oai::config::NRF_CONFIG_NAME)->get_sbi(),
          smf_cfg->enable_tls());
  nrf_uri +=
      "?target-nf-type=NWDAF&requester-nf-type=SMF"
      "&service-names=nnwdaf-analyticsinfo";

  request req = {};
  req.uri     = nrf_uri;
  response resp = http_client_inst->send_http_request(method_e::GET, req);

  if (resp.status_code != http_status_code::OK) {
    Logger::smf_app().warn(
        "NWDAF discovery: NRF returned HTTP %d", resp.status_code);
    return false;
  }

  json json_data = {};
  try {
    json_data = json::parse(resp.body);
  } catch (const json::exception& e) {
    Logger::smf_app().warn(
        "NWDAF discovery: could not parse NRF response (%s)", e.what());
    return false;
  }

  if (json_data.find("nfInstances") == json_data.end() ||
      !json_data["nfInstances"].is_array() ||
      json_data["nfInstances"].empty()) {
    Logger::smf_app().warn(
        "NWDAF discovery: NRF returned no NWDAF instance. If the NWDAF did "
        "register, check that the NRF stores nfType=NWDAF rather than "
        "UNKNOWN.");
    return false;
  }

  for (const auto& instance : json_data["nfInstances"]) {
    if (instance.find("nfServices") == instance.end()) continue;
    for (const auto& service : instance["nfServices"]) {
      if (service.find("serviceName") == service.end()) continue;
      if (service["serviceName"].get<std::string>() != "nnwdaf-analyticsinfo")
        continue;
      std::string scheme = "http";
      if (service.find("scheme") != service.end() &&
          service["scheme"].is_string())
        scheme = service["scheme"].get<std::string>();
      if (service.find("ipEndPoints") == service.end()) continue;
      for (const auto& ep : service["ipEndPoints"]) {
        if (ep.find("ipv4Address") == ep.end()) continue;
        const std::string addr = ep["ipv4Address"].get<std::string>();
        int port               = 8080;
        if (ep.find("port") != ep.end() && ep["port"].is_number())
          port = ep["port"].get<int>();
        api_root = scheme + "://" + addr + ":" + std::to_string(port);
        Logger::smf_app().info(
            "NWDAF discovery: selected NWDAF instance %s at %s",
            instance.value("nfInstanceId", "?").c_str(), api_root.c_str());
        return true;
      }
    }
  }
  Logger::smf_app().warn(
      "NWDAF discovery: an NWDAF was returned but none advertised a reachable "
      "nnwdaf-analyticsinfo endpoint");
  return false;
}

//------------------------------------------------------------------------------
// TS 23.288 clause 6.2.2.4 / TS 23.502 clause 4.17.4 - discover the target UPF
// so the clause 6.5.1 Analytics Filter names a REAL NF Instance ID.
//
// The previous behaviour took this from SMF_NWDAF_TARGET_UPF_NF_INSTANCE_ID.
// The deployed value was a REAL UPF NF Instance ID from an earlier boot that had
// since gone stale - the OAI UPF regenerates it on every start - and by the time
// this was found the NRF no longer held it. The NWDAF was pinned to the same
// stale constant, so the filter round-trip agreed for the wrong reason and would
// have broken the moment either side was corrected on its own.
//
// SELECTION: the first UPF profile the NRF returns, which is exactly what
// process_upf_profile() picks for the PFCP association, so the analytics this
// consumer requests describe the UPF its own sessions actually traverse.
//
// KNOWN UPSTREAM DEFECT this has to live with: the OAI UPF generates a NEW
// nfInstanceId on every start and never calls NFDeregister (TS 29.510 clause
// 5.2.2.2), so several REGISTERED profiles describe one physical UPF. Resolving
// on every poll rather than once is what keeps this correct across a restart.
bool smf_nwdaf_consumer::discover_upf_nf_instance_id(
    std::string& nf_instance_id) {
  std::string nrf_uri =
      oai::smf::api::smf_sbi_helper::get_nrf_disc_search_nf_instances_uri(
          smf_cfg->get_nf(oai::config::NRF_CONFIG_NAME)->get_sbi(),
          smf_cfg->enable_tls());
  nrf_uri += "?target-nf-type=UPF&requester-nf-type=SMF";

  request req   = {};
  req.uri       = nrf_uri;
  response resp = http_client_inst->send_http_request(method_e::GET, req);

  if (resp.status_code != http_status_code::OK) {
    Logger::smf_app().warn(
        "NWDAF consumer: UPF discovery returned HTTP %d", resp.status_code);
    return false;
  }
  json json_data = {};
  try {
    json_data = json::parse(resp.body);
  } catch (const json::exception& e) {
    Logger::smf_app().warn(
        "NWDAF consumer: could not parse the NRF UPF response (%s)", e.what());
    return false;
  }
  if (json_data.find("nfInstances") == json_data.end() ||
      !json_data["nfInstances"].is_array() ||
      json_data["nfInstances"].empty()) {
    Logger::smf_app().warn(
        "NWDAF consumer: the NRF returned no UPF instance, so no NF Instance "
        "ID is available for the clause 6.5.1 Analytics Filter");
    return false;
  }
  int matched = 0;
  std::string first;
  for (const auto& instance : json_data["nfInstances"]) {
    if (instance.find("nfInstanceId") == instance.end()) continue;
    if (!instance["nfInstanceId"].is_string()) continue;
    if (first.empty()) first = instance["nfInstanceId"].get<std::string>();
    matched++;
  }
  if (first.empty()) return false;
  if (matched > 1) {
    Logger::smf_app().debug(
        "NWDAF consumer: the NRF holds %d UPF profiles; using the first (%s), "
        "the same one process_upf_profile() associates with. The extras are "
        "stale registrations the OAI UPF never deregisters.",
        matched, first.c_str());
  }
  nf_instance_id = first;
  return true;
}

//------------------------------------------------------------------------------
// TS 23.288 clause 6.1.2.1 (Analytics Request) with the inputs of clause 6.5.1.
//
// TS 29.520 maps the request onto GET .../nnwdaf-analyticsinfo/v1/analytics
// with the Analytics Filter Information carried in the event-filter query
// parameter. The NF Instance ID list is sent because clause 6.5.1 defines it
// as the way to scope NF load analytics to a particular NF - even though the
// NWDAF in this project currently ignores that filter and reports its single
// configured UPF, which is recorded as a producer-side gap rather than
// papered over by omitting the filter here.
bool smf_nwdaf_consumer::request_nf_load(
    const std::string& api_root, const std::string& target_nf_instance_id,
    std::vector<nwdaf_nf_load_t>& out, bool& nwdaf_reachable) {
  // nwdaf_reachable distinguishes "the NWDAF answered, it just has no data"
  // from "the NWDAF could not be reached". Only the latter justifies dropping
  // the cached apiRoot and rediscovering.
  nwdaf_reachable = false;
  const std::string event_filter =
      "%7B%22nfInstanceIds%22%3A%5B%22" + target_nf_instance_id + "%22%5D%7D";

  request req = {};
  req.uri     = api_root +
            "/nnwdaf-analyticsinfo/v1/analytics?event-id=NF_LOAD"
            "&event-filter=" +
            event_filter;

  response resp = http_client_inst->send_http_request(method_e::GET, req);
  if (resp.status_code == http_status_code::NO_CONTENT) {
    nwdaf_reachable = true;
    // TS 29.520, GET /analytics: "204 - No Content. The requested NWDAF
    // Analytics data does not exist." That is a WELL-FORMED answer, not a
    // transport failure: the NWDAF is healthy and simply has nothing for the
    // designated NF instance right now. Treating it as an error would make the
    // caller drop its cached apiRoot and rediscover in a loop.
    Logger::smf_app().info(
        "Nnwdaf_AnalyticsInfo_Request (NF_LOAD): NWDAF has no analytics for NF "
        "instance %s (HTTP 204) - holding the current preference",
        target_nf_instance_id.c_str());
    return false;
  }
  if (resp.status_code != http_status_code::OK) {
    Logger::smf_app().warn(
        "Nnwdaf_AnalyticsInfo_Request (NF_LOAD): NWDAF returned HTTP %d",
        resp.status_code);
    return false;
  }
  nwdaf_reachable = true;

  json json_data = {};
  try {
    json_data = json::parse(resp.body);
  } catch (const json::exception& e) {
    Logger::smf_app().warn(
        "Nnwdaf_AnalyticsInfo_Request (NF_LOAD): could not parse response "
        "(%s)",
        e.what());
    return false;
  }

  if (json_data.find("nfLoadLevelInfos") == json_data.end() ||
      !json_data["nfLoadLevelInfos"].is_array()) {
    Logger::smf_app().warn(
        "Nnwdaf_AnalyticsInfo_Request (NF_LOAD): response carried no "
        "nfLoadLevelInfos");
    return false;
  }

  for (const auto& entry : json_data["nfLoadLevelInfos"]) {
    nwdaf_nf_load_t load = {};
    if (entry.find("nfInstanceId") != entry.end() &&
        entry["nfInstanceId"].is_string())
      load.nf_instance_id = entry["nfInstanceId"].get<std::string>();
    if (entry.find("nfCpuUsage") != entry.end() &&
        entry["nfCpuUsage"].is_number())
      load.nf_cpu_usage = entry["nfCpuUsage"].get<int32_t>();
    if (entry.find("nfMemoryUsage") != entry.end() &&
        entry["nfMemoryUsage"].is_number())
      load.nf_memory_usage = entry["nfMemoryUsage"].get<int32_t>();
    if (entry.find("nfLoadLevelAverage") != entry.end() &&
        entry["nfLoadLevelAverage"].is_number())
      load.nf_load_level_average = entry["nfLoadLevelAverage"].get<int32_t>();
    // TS 29.520 spells this attribute with a lowercase 'p'.
    if (entry.find("nfLoadLevelpeak") != entry.end() &&
        entry["nfLoadLevelpeak"].is_number())
      load.nf_load_level_peak = entry["nfLoadLevelpeak"].get<int32_t>();
    // Confidence exists ONLY for predictions (TS 23.288 Table 6.5.3-2). If the
    // NWDAF did not send one, none is invented.
    if (entry.find("confidence") != entry.end() &&
        entry["confidence"].is_number()) {
      load.confidence     = entry["confidence"].get<int32_t>();
      load.has_confidence = true;
    }
    out.push_back(load);
  }
  return !out.empty();
}

//------------------------------------------------------------------------------
// The steering policy. Deterministic and expressed only in terms of values the
// NWDAF actually reported.
//
// nfLoadLevelAverage is TS 23.288 Table 6.5.3-1 "NF load": "The average load of
// the NF instance over the Analytics target period". It is used directly; no
// score is synthesised and no confidence is assumed.
bool smf_nwdaf_consumer::decide(
    const nwdaf_nf_load_t& load, std::string& dnai, std::string& reason) {
  if (load.nf_load_level_average < 0) {
    reason = "NWDAF reported no nfLoadLevelAverage - holding current path";
    return false;
  }
  if (load.nf_load_level_average >= m_load_threshold) {
    m_clear_streak = 0;
    dnai           = m_dnai_congested;
    reason = "nfLoadLevelAverage " + std::to_string(load.nf_load_level_average) +
             " >= threshold " + std::to_string(m_load_threshold);
    return true;
  }
  m_clear_streak++;
  if (m_clear_streak < m_clear_streak_required) {
    reason = "nfLoadLevelAverage " + std::to_string(load.nf_load_level_average) +
             " < threshold " + std::to_string(m_load_threshold) +
             " but clear streak " + std::to_string(m_clear_streak) + "/" +
             std::to_string(m_clear_streak_required) + " - holding";
    return false;
  }
  dnai   = m_dnai_normal;
  reason = "nfLoadLevelAverage " + std::to_string(load.nf_load_level_average) +
           " < threshold " + std::to_string(m_load_threshold) + " for " +
           std::to_string(m_clear_streak) + " consecutive reports";
  return true;
}

//------------------------------------------------------------------------------
// TS 23.288 clause 6.1.2.1 (Analytics Request) with the output of clause 6.14.3.
//
// TS 29.520 maps this onto GET .../nnwdaf-analyticsinfo/v1/analytics with
// event-id=DN_PERFORMANCE. The response envelope carries dnPerfInfos, each
// entry holding a dnPerf[] array whose members are the per-path readings of
// Table 6.14.3-1.
//
// NO Analytics Filter is sent. Clause 6.14.1 permits filtering by DNAI, Anchor
// UPF info, Application ID, S-NSSAI, DNN and AS address, but this consumer
// wants EVERY DNAI the NWDAF has data for - that is the whole point of asking.
bool smf_nwdaf_consumer::request_dn_performance(
    const std::string& api_root, std::vector<nwdaf_dn_perf_t>& out,
    bool& nwdaf_reachable) {
  nwdaf_reachable = false;

  request req = {};
  req.uri = api_root + "/nnwdaf-analyticsinfo/v1/analytics?event-id=DN_PERFORMANCE";
  if (m_predict_sec > 0) {
    // TS 29.520 EventReportingRequirement.offsetPeriod - a POSITIVE value asks
    // for a prediction that far into the future. Percent-encoded because it is
    // a JSON object in a query parameter.
    req.uri += "&ana-req=%7B%22offsetPeriod%22%3A" +
               std::to_string(m_predict_sec) + "%7D";
  }

  response resp = http_client_inst->send_http_request(method_e::GET, req);
  if (resp.status_code == http_status_code::NO_CONTENT) {
    nwdaf_reachable = true;
    // Same reasoning as the NF_LOAD path: TS 29.520 defines 204 as "The
    // requested NWDAF Analytics data does not exist", which is a healthy
    // answer, not a transport failure.
    Logger::smf_app().info(
        "Nnwdaf_AnalyticsInfo_Request (DN_PERFORMANCE): NWDAF has no DN "
        "performance analytics (HTTP 204) - holding the current preference");
    return false;
  }
  if (resp.status_code != http_status_code::OK) {
    Logger::smf_app().warn(
        "Nnwdaf_AnalyticsInfo_Request (DN_PERFORMANCE): NWDAF returned HTTP %d",
        resp.status_code);
    return false;
  }
  nwdaf_reachable = true;

  json json_data = {};
  try {
    json_data = json::parse(resp.body);
  } catch (const json::exception& e) {
    Logger::smf_app().warn(
        "Nnwdaf_AnalyticsInfo_Request (DN_PERFORMANCE): could not parse "
        "response (%s)",
        e.what());
    return false;
  }

  if (json_data.find("dnPerfInfos") == json_data.end() ||
      !json_data["dnPerfInfos"].is_array()) {
    Logger::smf_app().warn(
        "Nnwdaf_AnalyticsInfo_Request (DN_PERFORMANCE): response carried no "
        "dnPerfInfos");
    return false;
  }

  for (const auto& info : json_data["dnPerfInfos"]) {
    std::string dnn = {};
    if (info.find("dnn") != info.end() && info["dnn"].is_string())
      dnn = info["dnn"].get<std::string>();

    if (info.find("dnPerf") == info.end() || !info["dnPerf"].is_array())
      continue;

    for (const auto& entry : info["dnPerf"]) {
      nwdaf_dn_perf_t perf = {};
      perf.dnn             = dnn;
      if (entry.find("dnai") != entry.end() && entry["dnai"].is_string())
        perf.dnai = entry["dnai"].get<std::string>();
      if (entry.find("upfId") != entry.end() && entry["upfId"].is_string())
        perf.upf_id = entry["upfId"].get<std::string>();

      // An entry with no DNAI cannot be ranked against another path - the DNAI
      // IS the identity of the path here. Skip rather than guess.
      if (perf.dnai.empty()) {
        Logger::smf_app().debug(
            "DN_PERFORMANCE: dnPerf entry with no dnai - ignoring");
        continue;
      }

      if (entry.find("perfData") != entry.end() &&
          entry["perfData"].is_object()) {
        const auto& pd = entry["perfData"];
        if (pd.find("avgTrafficRate") != pd.end() &&
            pd["avgTrafficRate"].is_string()) {
          perf.has_avg_traffic_rate = parse_bitrate_bps(
              pd["avgTrafficRate"].get<std::string>(),
              perf.avg_traffic_rate_bps);
        }
        if (pd.find("maxTrafficRate") != pd.end() &&
            pd["maxTrafficRate"].is_string()) {
          perf.has_max_traffic_rate = parse_bitrate_bps(
              pd["maxTrafficRate"].get<std::string>(),
              perf.max_traffic_rate_bps);
        }
        // avePacketDelay / maxPacketDelay / avgPacketLossRate are deliberately
        // NOT parsed. This deployment cannot measure them (TS 23.288 Table
        // 6.4.2-2 NOTE 1: how the NWDAF collects QoS flow packet delay from the
        // UPF "is not defined in this Release"), so the producer omits them and
        // this consumer has no field to put them in.
      }

      // PROJECT-SPECIFIC vendor extension. Deliberately read from OUTSIDE
      // perfData: it is an AF_PACKET transmit-stall indicator, not one of
      // Table 6.14.3-1's PerfData attributes, and folding it in would assert
      // an equivalence with avgPacketLossRate that the measurements disprove.
      if (entry.find("oaiPathHealthExt") != entry.end() &&
          entry["oaiPathHealthExt"].is_object()) {
        const auto& h = entry["oaiPathHealthExt"];
        if (h.find("state") != h.end() && h["state"].is_string())
          perf.health_state = h["state"].get<std::string>();
        // A JSON null here is the producer saying "no observation", and it
        // MUST stay unknown rather than becoming 0.0 - see the header.
        if (h.find("sendtoFailurePerPacket") != h.end() &&
            h["sendtoFailurePerPacket"].is_number()) {
          perf.health_ratio     = h["sendtoFailurePerPacket"].get<double>();
          perf.has_health_ratio = true;
        }
        if (h.find("txAttempts") != h.end() && h["txAttempts"].is_number())
          perf.health_tx_attempts = h["txAttempts"].get<int64_t>();
        if (h.find("observedAt") != h.end() && h["observedAt"].is_number()) {
          perf.health_observed_at     = h["observedAt"].get<int64_t>();
          perf.has_health_observed_at = true;
        }
        if (h.find("ageSec") != h.end() && h["ageSec"].is_number()) {
          perf.health_age_sec = h["ageSec"].get<int64_t>();
          perf.has_health_age = true;
        }
      }

      // TS 23.288 Table 6.14.3-2: Confidence exists ONLY in predictions. It is
      // carried on the DnPerfInfo (the (appId, S-NSSAI, DNN) grouping), not on
      // the per-path DnPerf, so it is read from the enclosing object.
      if (info.find("confidence") != info.end() &&
          info["confidence"].is_number()) {
        perf.confidence     = info["confidence"].get<int32_t>();
        perf.has_confidence = true;
      }

      out.push_back(perf);
    }
  }

  return !out.empty();
}

//------------------------------------------------------------------------------
// See the header for the honest ceiling on what this can and cannot conclude.
bool smf_nwdaf_consumer::decide_from_dn_performance(
    const std::vector<nwdaf_dn_perf_t>& perfs, std::string& dnai,
    std::string& reason) {
  // Keep only entries that carry a MEASURED average traffic rate. An absent
  // rate is unknown, not zero.
  std::vector<const nwdaf_dn_perf_t*> ranked = {};
  int dropped_low_confidence = 0;
  for (const auto& p : perfs) {
    if (!p.has_avg_traffic_rate) continue;
    // A PREDICTION carries a Confidence (TS 23.288 Table 6.14.3-2) and is only
    // acted on when it clears the configured floor. A STATISTIC carries none
    // and is NOT judged as if it did - absent is not zero, and a statistic is
    // not a low-confidence prediction.
    if (p.has_confidence && p.confidence < m_min_confidence) {
      ++dropped_low_confidence;
      continue;
    }
    ranked.push_back(&p);
  }

  if (dropped_low_confidence > 0) {
    Logger::smf_app().info(
        "NWDAF DN_PERFORMANCE: ignored %d predicted DNAI(s) below the "
        "configured confidence floor of %d",
        dropped_low_confidence, m_min_confidence);
  }

  if (ranked.empty()) {
    reason =
        "NWDAF reported no measured avgTrafficRate for any DNAI - holding "
        "current path";
    return false;
  }

  if (ranked.size() == 1) {
    // THE structural limitation, hit in the single-UPF topology: only the DNAI
    // the session is actually on has data. There is nothing to compare it
    // against, and the absent DNAI must NOT be assumed worse.
    reason = "only one DNAI ('" + ranked.front()->dnai +
             "') has measured DN performance (" +
             bitrate_to_string(ranked.front()->avg_traffic_rate_bps) +
             ") - nothing to rank against, holding current path. This is the "
             "expected outcome with a single active path; a comparison needs "
             "concurrently active paths (multi-UPF).";
    return false;
  }

  const nwdaf_dn_perf_t* best = ranked.front();
  for (const auto* p : ranked) {
    if (p->avg_traffic_rate_bps > best->avg_traffic_rate_bps) best = p;
  }

  const std::string current = get_preferred_dnai();
  if (best->dnai == current) {
    reason = "DNAI '" + best->dnai + "' still ranks highest (" +
             bitrate_to_string(best->avg_traffic_rate_bps) + ")";
    dnai   = best->dnai;
    return true;
  }

  // PROJECT-SPECIFIC hysteresis, not 3GPP: the two rates come from different
  // observation windows with different offered load (design section 4.1), so a
  // small difference carries no information. Require a clear margin before
  // moving the preference.
  if (!current.empty()) {
    const nwdaf_dn_perf_t* current_perf = nullptr;
    for (const auto* p : ranked) {
      if (p->dnai == current) current_perf = p;
    }
    if (current_perf) {
      const double threshold = current_perf->avg_traffic_rate_bps *
                               (1.0 + (double) m_dnperf_margin_percent / 100.0);
      if (best->avg_traffic_rate_bps < threshold) {
        reason = "DNAI '" + best->dnai + "' (" +
                 bitrate_to_string(best->avg_traffic_rate_bps) +
                 ") does not beat current '" + current + "' (" +
                 bitrate_to_string(current_perf->avg_traffic_rate_bps) +
                 ") by the required " +
                 std::to_string(m_dnperf_margin_percent) +
                 "% margin - holding";
        return false;
      }
    }
  }

  dnai = best->dnai;
  const std::string kind =
      best->has_confidence ? "predicted" : "measured";
  reason = "DNAI '" + best->dnai + "' has the highest " + kind +
           " avgTrafficRate (" +
           bitrate_to_string(best->avg_traffic_rate_bps) + ") of " +
           std::to_string(ranked.size()) + " DNAIs with data";
  if (best->has_confidence) {
    reason += ", confidence " + std::to_string(best->confidence) +
              " (>= floor " + std::to_string(m_min_confidence) + ")";
  }
  return true;
}

//------------------------------------------------------------------------------
bool smf_nwdaf_consumer::health_is_fresh(const nwdaf_dn_perf_t& p) const {
  // No age at all means the producer sent no extension, or an incomplete one.
  // A sample whose age is unknown is never acted on.
  if (!p.has_health_age) return false;
  if (m_health_max_age_sec <= 0) return true;  // bound disabled

  // `ageSec` is how old the sample was WHEN IT WAS FETCHED, and it never grows
  // afterwards. If the NWDAF becomes unreachable the cached readings would
  // therefore stay "fresh" forever and the consumer would keep steering on a
  // picture of the network that stopped updating. Add the time elapsed since
  // the fetch so a cache that stops being refreshed ages out on its own and
  // everything falls back to UNKNOWN -> HOLD, which is the safe direction.
  int64_t effective_age = p.health_age_sec;
  if (m_last_perfs_at > 0) {
    const int64_t since_fetch =
        static_cast<int64_t>(std::time(nullptr)) - m_last_perfs_at;
    if (since_fetch > 0) effective_age += since_fetch;
  }
  return effective_age <= (int64_t) m_health_max_age_sec;
}

//------------------------------------------------------------------------------
void smf_nwdaf_consumer::update_degraded_memory(
    const std::vector<nwdaf_dn_perf_t>& perfs) {
  const int64_t now = static_cast<int64_t>(std::time(nullptr));

  for (const auto& p : perfs) {
    if (p.dnai.empty() || !health_is_fresh(p)) continue;

    if (p.health_state == "OBSERVED_DEGRADED") {
      m_dnai_degraded_at[p.dnai] = now;
    } else if (p.health_state == "OBSERVED_HEALTHY") {
      // Direct evidence of recovery clears the suspicion immediately. Only a
      // POSITIVE observation may do this - an UNKNOWN_* state must not, because
      // an idle path reads UNKNOWN whether it is healthy or broken (28.6).
      if (m_dnai_degraded_at.erase(p.dnai) > 0) {
        Logger::smf_app().info(
            "NWDAF path health: DNAI '%s' observed healthy again - clearing "
            "its recently-degraded mark",
            p.dnai.c_str());
      }
    }
  }

  // Decay: forget a mark nothing has contradicted for long enough, so a path
  // that has simply been left alone becomes a candidate again.
  for (auto it = m_dnai_degraded_at.begin(); it != m_dnai_degraded_at.end();) {
    if (m_degraded_memory_sec > 0 &&
        (now - it->second) > (int64_t) m_degraded_memory_sec) {
      Logger::smf_app().info(
          "NWDAF path health: DNAI '%s' has not been seen degraded for %ds - "
          "eligible again",
          it->first.c_str(), m_degraded_memory_sec);
      it = m_dnai_degraded_at.erase(it);
    } else {
      ++it;
    }
  }
}

//------------------------------------------------------------------------------
// See the header for the rule and for why it is per-session.
bool smf_nwdaf_consumer::decide_for_session(
    const std::string& current_dnai, const std::set<std::string>& authorized,
    std::string& dnai, std::string& reason) {
  if (current_dnai.empty()) {
    reason =
        "the UPF has not confirmed a DNAI for this session yet - holding";
    return false;
  }

  std::vector<nwdaf_dn_perf_t> perfs = {};
  std::map<std::string, int64_t> degraded_at = {};
  {
    std::lock_guard<std::mutex> lock(m_mutex);
    perfs       = m_last_perfs;
    degraded_at = m_dnai_degraded_at;
  }
  if (perfs.empty()) {
    reason = "no DN_PERFORMANCE readings consumed yet - holding";
    return false;
  }

  // ---- 1. the gate: what is the path this session is actually on doing? ----
  const nwdaf_dn_perf_t* cur = nullptr;
  for (const auto& p : perfs) {
    if (p.dnai == current_dnai) cur = &p;
  }
  if (!cur) {
    reason = "NWDAF reported nothing about the current DNAI '" + current_dnai +
             "' - holding (absence is not evidence of a problem)";
    return false;
  }
  if (!health_is_fresh(*cur)) {
    reason = "path health for the current DNAI '" + current_dnai +
             "' is absent or too old (max " +
             std::to_string(m_health_max_age_sec) + "s) - holding";
    return false;
  }
  if (cur->health_state != "OBSERVED_DEGRADED") {
    // HEALTHY -> nothing to fix. UNKNOWN_* -> no information, and acting on no
    // information is exactly what this rule exists to avoid.
    reason = "current DNAI '" + current_dnai + "' is " +
             (cur->health_state.empty() ? "UNKNOWN (no health reported)"
                                        : cur->health_state) +
             " - holding (only OBSERVED_DEGRADED justifies a steer)";
    return false;
  }

  // ---- 2. candidates: authorized, not the current one, not known-bad -------
  const nwdaf_dn_perf_t* best = nullptr;
  int best_tier               = 99;   // 0 = OBSERVED_HEALTHY, 1 = UNKNOWN_*
  int skipped_degraded        = 0;
  int skipped_suspect         = 0;

  for (const auto& p : perfs) {
    if (p.dnai.empty() || p.dnai == current_dnai) continue;
    if (authorized.find(p.dnai) == authorized.end()) continue;

    const bool fresh = health_is_fresh(p);
    if (fresh && p.health_state == "OBSERVED_DEGRADED") {
      ++skipped_degraded;
      continue;
    }
    // Recently degraded and not since observed healthy. THIS is the ping-pong
    // guard: after a steer the abandoned path goes idle and reads
    // UNKNOWN_NO_TRAFFIC, which without this memory looks like a fresh, clean
    // candidate to steer straight back into.
    if (degraded_at.find(p.dnai) != degraded_at.end()) {
      ++skipped_suspect;
      continue;
    }

    const int tier =
        (fresh && p.health_state == "OBSERVED_HEALTHY") ? 0 : 1;

    if (!best || tier < best_tier) {
      best = &p; best_tier = tier; continue;
    }
    if (tier > best_tier) continue;

    // Same tier: least loaded wins. A rate below the confidence floor is
    // unusable, and a DNAI with no usable rate sorts LAST - unknown load is
    // not zero load.
    const bool p_usable =
        p.has_avg_traffic_rate &&
        !(p.has_confidence && p.confidence < m_min_confidence);
    const bool b_usable =
        best->has_avg_traffic_rate &&
        !(best->has_confidence && best->confidence < m_min_confidence);
    if (p_usable && !b_usable) {
      best = &p;
    } else if (p_usable && b_usable &&
               p.avg_traffic_rate_bps < best->avg_traffic_rate_bps) {
      best = &p;
    }
  }

  const std::string cur_ratio =
      cur->has_health_ratio ? std::to_string(cur->health_ratio) : "n/a";

  if (!best) {
    reason = "current DNAI '" + current_dnai +
             "' is OBSERVED_DEGRADED (stall ratio " + cur_ratio +
             ") but NO authorized alternative is usable (" +
             std::to_string(skipped_degraded) + " also degraded, " +
             std::to_string(skipped_suspect) +
             " recently degraded) - holding on a known-bad path is better than "
             "oscillating between two of them";
    return false;
  }

  dnai = best->dnai;
  reason = "current DNAI '" + current_dnai + "' is OBSERVED_DEGRADED (stall "
           "ratio " + cur_ratio + " over " +
           std::to_string(cur->health_tx_attempts) +
           " tx packets) - steering to '" + best->dnai + "' (" +
           (best_tier == 0 ? "OBSERVED_HEALTHY"
                           : (best->health_state.empty()
                                  ? "UNKNOWN"
                                  : best->health_state)) +
           ")";
  if (best_tier != 0) {
    reason +=
        ". NOTE: the target is NOT known to be healthy - this is leaving a "
        "known-bad path for an unknown one, which is deliberate; it is never "
        "read as 'assume healthy'";
  }
  return true;
}

//------------------------------------------------------------------------------
// One DN_PERFORMANCE poll. Deliberately a separate function so that the proven
// NF_LOAD body of poll_loop() keeps its exact shape.
void smf_nwdaf_consumer::poll_dn_performance(
    const std::string& api_root, bool& nwdaf_reachable) {
  std::vector<nwdaf_dn_perf_t> perfs = {};
  if (!request_dn_performance(api_root, perfs, nwdaf_reachable)) return;

  for (const auto& p : perfs) {
    Logger::smf_app().info(
        "NWDAF DN_PERFORMANCE analytics (%s): dnai=%s upfId=%s dnn=%s "
        "avgTrafficRate=%s maxTrafficRate=%s confidence=%s",
        p.has_confidence ? "PREDICTION, Table 6.14.3-2"
                         : "statistics, Table 6.14.3-1",
        p.dnai.c_str(), p.upf_id.empty() ? "<absent>" : p.upf_id.c_str(),
        p.dnn.empty() ? "<absent>" : p.dnn.c_str(),
        p.has_avg_traffic_rate
            ? bitrate_to_string(p.avg_traffic_rate_bps).c_str()
            : "not provided",
        p.has_max_traffic_rate
            ? bitrate_to_string(p.max_traffic_rate_bps).c_str()
            : "not provided",
        p.has_confidence ? std::to_string(p.confidence).c_str()
                         : "not provided (statistics)");
  }

  // Stash the readings so the per-session evaluation can consult them without
  // re-issuing the analytics request once per PDU session, and update the
  // decaying record of which DNAIs have been seen degraded.
  {
    std::lock_guard<std::mutex> lock(m_mutex);
    m_last_perfs    = perfs;
    m_last_perfs_at = static_cast<int64_t>(std::time(nullptr));
  }
  update_degraded_memory(perfs);

  for (const auto& p : perfs) {
    if (p.health_state.empty()) continue;
    // confidence is carried on this line too, purely so ONE grep shows the whole
    // per-DNAI picture. It is NOT an input to the HEALTH rule - it describes the
    // stability of the avgTrafficRate SERIES (TS 23.288 Table 6.14.3-2), and this
    // rule does not rank on rate. It gates only the tie-break between two
    // same-tier candidates, and under the RATE rule every decision.
    Logger::smf_app().info(
        "NWDAF path health (vendor extension, NOT 3GPP): dnai=%s state=%s "
        "stallPerPacket=%s txPackets=%s ageSec=%s | rate=%s confidence=%s "
        "(rate+confidence are NOT used by the HEALTH rule)",
        p.dnai.c_str(), p.health_state.c_str(),
        p.has_health_ratio ? std::to_string(p.health_ratio).c_str()
                           : "null (no observation - NOT zero)",
        p.health_tx_attempts >= 0 ? std::to_string(p.health_tx_attempts).c_str()
                                  : "absent",
        p.has_health_age ? std::to_string(p.health_age_sec).c_str() : "absent",
        p.has_avg_traffic_rate
            ? bitrate_to_string(p.avg_traffic_rate_bps).c_str()
            : "not provided",
        p.has_confidence ? std::to_string(p.confidence).c_str()
                         : "absent (statistics carry none)");
  }

  // Under the HEALTH rule there is no single global preference to form: the
  // decision depends on which path each SESSION is on, and is taken in
  // evaluate_sessions() where that is known.
  if (m_dnperf_rule == "HEALTH") return;

  std::string dnai   = {};
  std::string reason = {};
  if (!decide_from_dn_performance(perfs, dnai, reason)) {
    Logger::smf_app().debug("NWDAF analytics preference: %s", reason.c_str());
    return;
  }

  std::string previous = {};
  {
    std::lock_guard<std::mutex> lock(m_mutex);
    previous         = m_preferred_dnai;
    m_preferred_dnai = dnai;
  }
  if (previous != dnai) {
    Logger::smf_app().info(
        "NWDAF analytics preference: %s -> %s (%s). This is a PREFERENCE only "
        "- it is applied per session and only if the PCF authorized it. It is "
        "a MEASUREMENT of paths already used, not a prediction of an unused "
        "path.",
        previous.empty() ? "<none>" : previous.c_str(), dnai.c_str(),
        reason.c_str());
  } else {
    Logger::smf_app().debug(
        "NWDAF analytics preference unchanged (%s): %s", dnai.c_str(),
        reason.c_str());
  }
}

//------------------------------------------------------------------------------
std::string smf_nwdaf_consumer::get_preferred_dnai() {
  std::lock_guard<std::mutex> lock(m_mutex);
  return m_preferred_dnai;
}

//------------------------------------------------------------------------------
// The authorization gate. Everything the analytics produce has to pass through
// here before it can influence a session.
std::string smf_nwdaf_consumer::choose_authorized_dnai(
    const std::set<std::string>& authorized, std::string& reason) {
  const std::string preferred = get_preferred_dnai();

  if (preferred.empty()) {
    reason = "no NWDAF analytics consumed yet - keeping current path";
    return {};
  }
  if (authorized.empty()) {
    // No policy data at all. The SMF has no authority of its own to steer.
    reason = "PCF authorized no DNAI for this session - not steering";
    return {};
  }
  if (authorized.size() == 1) {
    // Exactly one authorized DNAI: there is no choice to make, so there is
    // nothing for analytics to influence.
    reason =
        "only one PCF-authorized DNAI (" + *authorized.begin() +
        ") - no selection for analytics to make";
    return {};
  }
  if (authorized.find(preferred) == authorized.end()) {
    // THE case this whole change exists to prevent: the analytics prefer a
    // DNAI the PCF never authorized. The PCF wins, always.
    std::string list;
    for (const auto& d : authorized) {
      if (!list.empty()) list += ", ";
      list += d;
    }
    reason = "NWDAF-preferred DNAI '" + preferred +
             "' is NOT authorized by the PCF (authorized: " + list +
             ") - refusing to steer";
    return {};
  }
  reason = "NWDAF-preferred DNAI '" + preferred + "' is PCF-authorized";
  return preferred;
}

//------------------------------------------------------------------------------
// Per-session evaluation. The authorized DNAI set is a property of the SESSION
// (its own SmPolicyDecision), not of the SMF, so a single global answer would
// be wrong the moment two UEs have different policies - which is exactly the
// case here (decision_supi1 vs decision_supi2).
void smf_nwdaf_consumer::evaluate_sessions() {
  if (!smf_app_inst) return;
  std::vector<std::shared_ptr<oai::app::smf::smf_context>> contexts = {};
  smf_app_inst->get_smf_contexts(contexts);

  // ONE budget for the whole cycle. poll_loop() calls this function exactly
  // once per poll (both the DN_PERFORMANCE and the NF_LOAD branch do), so a
  // counter local to this call is genuinely global to the steering engine -
  // which is precisely what a per-session cooldown cannot be.
  steer_cycle_state cycle = {};
  cycle.max_per_cycle       = m_max_steers_per_cycle;
  cycle.last_global_steer_at = m_last_global_steer_at;
  int eligible = 0;

  for (const auto& sc : contexts) {
    if (!sc) continue;
    // pdu_session_id_t is uint8_t, not uint32_t - get_pdu_sessions() takes a
    // non-const reference so the key type has to match exactly.
    std::map<pdu_session_id_t, std::shared_ptr<oai::app::smf::smf_pdu_session>>
        sessions = {};
    sc->get_pdu_sessions(sessions);

    for (const auto& entry : sessions) {
      const std::shared_ptr<oai::app::smf::smf_pdu_session>& sp = entry.second;
      if (!sp) continue;

      if (!sp->policy_ptr) {
        // No PCF policy association: no authorization, therefore no steering.
        Logger::smf_app().debug(
            "NWDAF per-session: PDU session %d has no PCF policy association "
            "- not steering",
            static_cast<int>(entry.first));
        continue;
      }

      const auto by_precedence =
          oai::app::smf::get_authorized_dnais(sp->policy_ptr->decision);
      if (by_precedence.empty()) {
        Logger::smf_app().debug(
            "NWDAF per-session: PDU session %d - PCF decision authorizes no "
            "DNAI",
            static_cast<int>(entry.first));
        continue;
      }
      // Lower precedence value wins (TS 29.512), so the first entry is the
      // governing rule. Analytics select WITHIN it and never across it, so an
      // AF-installed rule at precedence 1 still out-ranks an analytics
      // preference expressed against the provisioned rule at precedence 10.
      const uint32_t best_precedence      = by_precedence.begin()->first;
      const std::set<std::string>& allowed = by_precedence.begin()->second;

      std::string reason = {};
      std::string chosen = {};
      if (m_dnperf_rule == "HEALTH") {
        // Read the CONFIRMED DNAI - what the UPF has acknowledged - not the
        // intent. A steer whose PFCP update failed would otherwise look like a
        // success forever.
        std::string confirmed = {};
        if (sp->get_session_handler() &&
            sp->get_session_handler()->get_session_graph()) {
          confirmed = sp->get_session_handler()
                          ->get_session_graph()
                          ->get_n6_confirmed_dnai();
        }
        std::string target = {};
        if (decide_for_session(confirmed, allowed, target, reason)) {
          // COOLDOWN. m_last_trigger only suppresses a repeat of the SAME
          // target, which does nothing against A->B->A oscillation. This bounds
          // how often a session may be moved at all.
          const int64_t now = static_cast<int64_t>(std::time(nullptr));
          const uint64_t scid = sp->policy_ptr ? sp->policy_ptr->id : 0;
          auto it = m_last_steer_at.find(scid);
          if (scid != 0 && it != m_last_steer_at.end() &&
              m_steer_cooldown_sec > 0 &&
              (now - it->second) < (int64_t) m_steer_cooldown_sec) {
            reason = "steer to '" + target + "' suppressed: this session was "
                     "steered " + std::to_string(now - it->second) +
                     "s ago and the cooldown is " +
                     std::to_string(m_steer_cooldown_sec) + "s";
          } else {
            chosen = target;
            if (scid != 0) m_last_steer_at[scid] = now;
          }
        }
      } else {
        chosen = choose_authorized_dnai(allowed, reason);
      }

      std::string list;
      for (const auto& d : allowed) {
        if (!list.empty()) list += ", ";
        list += d;
      }
      // GLOBAL SERIALIZATION. A session that is otherwise ready to move is
      // held here if the cycle's budget is spent, or if the target's health has
      // not been re-observed since the previous steer. Applied AFTER the PCF
      // authorization check, so it can never widen what policy allows - it only
      // ever withholds a move.
      if (!chosen.empty()) {
        ++eligible;
        bool target_has_health          = false;
        int64_t target_health_observed  = 0;
        {
          std::lock_guard<std::mutex> lock(m_mutex);
          for (const auto& p : m_last_perfs) {
            if (p.dnai == chosen && p.has_health_observed_at) {
              target_has_health         = true;
              target_health_observed    = p.health_observed_at;
              break;
            }
          }
        }
        std::string hold = {};
        if (!serialization_allows_steer(cycle, target_has_health,
                                        target_health_observed, hold)) {
          Logger::smf_app().info(
              "Steering cycle: holding PDU session %d (target '%s') - %s",
              static_cast<int>(entry.first), chosen.c_str(), hold.c_str());
          chosen.clear();
          reason = hold;
        }
      }

      if (chosen.empty()) {
        Logger::smf_app().info(
            "NWDAF per-session decision: PDU session %d (precedence %u, "
            "authorized {%s}) -> HOLD: %s",
            static_cast<int>(entry.first), best_precedence, list.c_str(),
            reason.c_str());
      } else {
        Logger::smf_app().info(
            "NWDAF per-session decision: PDU session %d (precedence %u, "
            "authorized {%s}) -> SELECT '%s': %s",
            static_cast<int>(entry.first), best_precedence, list.c_str(),
            chosen.c_str(), reason.c_str());
        // The decision used to end here - computed, authorized, and discarded.
        // That left the loop open.
        if (maybe_trigger_steering(sp, chosen, static_cast<int>(entry.first))) {
          const int64_t now = static_cast<int64_t>(std::time(nullptr));
          ++cycle.steers_this_cycle;
          cycle.last_global_steer_at = now;
          m_last_global_steer_at     = now;
          Logger::smf_app().info(
              "Steering cycle: steered PDU session %d to '%s' (%d/%d this "
              "cycle) - remaining sessions wait for the next cycle and a "
              "health observation that postdates this move",
              static_cast<int>(entry.first), chosen.c_str(),
              cycle.steers_this_cycle, cycle.max_per_cycle);
        }
      }
    }
  }

  // Silent when nothing was eligible, so an idle deployment logs nothing new.
  if (eligible > 0) {
    Logger::smf_app().info(
        "Steering cycle: %d eligible session(s), %d steered (limit %d/cycle)",
        eligible, cycle.steers_this_cycle, cycle.max_per_cycle);
  }
}

//------------------------------------------------------------------------------
// See the header for why this only posts an ITTI message and never touches the
// session graph itself.
bool smf_nwdaf_consumer::maybe_trigger_steering(
    const std::shared_ptr<oai::app::smf::smf_pdu_session>& sp,
    const std::string& chosen, int pdu_session_id) {
  if (!m_act_enabled) return false;  // default: decide and log, never act
  if (!sp || chosen.empty()) return false;
  if (!smf_app_inst || !itti_inst) return false;

  if (!sp->policy_ptr) {
    // Cannot happen on this path (the caller already required it), but the
    // scid is read from it below, so never dereference on trust.
    return false;
  }
  if (!sp->get_session_handler() ||
      !sp->get_session_handler()->get_session_graph()) {
    Logger::smf_app().debug(
        "SMF-initiated steering: PDU session %d has no session graph - not "
        "triggering",
        pdu_session_id);
    return false;
  }

  // DAMPING 1: the session is already where the analytics want it. This is the
  // steady state and must be silent, not a modification every 10 s.
  //
  // Read the CONFIRMED DNAI - what the UPF has acknowledged - not the intent.
  // Reading the intent means a steer whose PFCP update failed looks like a
  // success forever: intent says the new DNAI, so this check concludes "already
  // there" and never retries, while the UPF keeps forwarding on the old path.
  const std::string current =
      sp->get_session_handler()->get_session_graph()->get_n6_confirmed_dnai();
  if (current == chosen) {
    // Reached the target: drop any damping state for this session so the map
    // does not accumulate an entry per SCID for the lifetime of the process.
    if (sp->policy_ptr) m_last_trigger.erase(sp->policy_ptr->id);
    return false;
  }

  // The SM Context ID, as stored when the policy association was created
  // (smf_context.cpp: sp->policy_ptr->id = smreq->scid).
  const uint64_t scid = sp->policy_ptr->id;
  if (scid == 0) {
    Logger::smf_app().debug(
        "SMF-initiated steering: PDU session %d has no SM Context ID - not "
        "triggering",
        pdu_session_id);
    return false;
  }

  // DAMPING 2: do not re-issue the same request while the previous one may
  // still be in flight. Applying a steer is not instantaneous, so between
  // sending the ITTI message and the graph being re-bound the DNAI comparison
  // above would still say "different" and fire again on the next tick.
  const int64_t now = static_cast<int64_t>(std::time(nullptr));
  const int64_t quiet_for = m_poll_sec > 0 ? (2 * m_poll_sec) : 20;
  auto it = m_last_trigger.find(scid);
  if (it != m_last_trigger.end() && it->second.first == chosen &&
      (now - it->second.second) < quiet_for) {
    Logger::smf_app().debug(
        "SMF-initiated steering: a request for DNAI '%s' on SM Context ID %lu "
        "was sent %lds ago - waiting for it to take effect",
        chosen.c_str(), (unsigned long) scid,
        (long) (now - it->second.second));
    return false;
  }
  m_last_trigger[scid] = std::make_pair(chosen, now);

  // The promise id is generated but NO promise is registered against it. Every
  // response/error path ends in smf_app::make_future_ready(), which is guarded
  // by `if (promises.count(pid) > 0)`, so an unregistered id is a safe no-op.
  // A self-initiated request has no HTTP caller waiting for a reply.
  const uint32_t pid = smf_app_inst->generate_promise_id();

  std::shared_ptr<itti_sbi_update_sm_context_request> itti_msg =
      std::make_shared<itti_sbi_update_sm_context_request>(
          TASK_SMF_APP, TASK_SMF_APP, pid, std::to_string(scid));
  itti_msg->http_version           = 1;
  itti_msg->steering_dnai          = chosen;
  // session_management_procedures_type_e is a GLOBAL-scope enum class in
  // src/common/smf.h, not a namespaced one.
  itti_msg->session_procedure_type = session_management_procedures_type_e::
      PDU_SESSION_MODIFICATION_SMF_REQUESTED;

  Logger::smf_app().info(
      "SMF-initiated steering: asking SMF-APP to move PDU session %d "
      "(SM Context ID %lu) from '%s' to '%s' - no AF and no PCF notification "
      "involved (TS 23.288 clause 6.14.4)",
      pdu_session_id, (unsigned long) scid,
      current.empty() ? "<unknown>" : current.c_str(), chosen.c_str());

  if (itti_inst->send_msg(itti_msg) != RETURNok) {
    Logger::smf_app().error(
        "SMF-initiated steering: could not send ITTI message %s to TASK_SMF_APP",
        itti_msg->get_msg_name());
    m_last_trigger.erase(scid);
    return false;
  }
  return true;
}

//------------------------------------------------------------------------------
void smf_nwdaf_consumer::poll_loop() {
  // Give the SMF time to finish its own NRF registration before the first
  // discovery attempt, otherwise the very first query races NF startup.
  std::this_thread::sleep_for(std::chrono::seconds(10));

  while (m_running) {
    std::string api_root = {};
    {
      std::lock_guard<std::mutex> lock(m_mutex);
      api_root = m_cached_api_root;
    }

    if (api_root.empty()) {
      if (!discover_nwdaf(api_root)) {
        // A statically configured URI is a fallback for bring-up and for
        // deployments whose NRF cannot store nfType=NWDAF. It is NOT the
        // clause 5.2 mechanism and is logged as such.
        if (!m_static_uri.empty()) {
          api_root = m_static_uri;
          Logger::smf_app().warn(
              "NWDAF discovery failed, falling back to statically configured "
              "SMF_NWDAF_URI=%s (not TS 23.288 clause 5.2 discovery)",
              api_root.c_str());
        } else {
          std::this_thread::sleep_for(std::chrono::seconds(m_poll_sec));
          continue;
        }
      }
      std::lock_guard<std::mutex> lock(m_mutex);
      m_cached_api_root = api_root;
    }

    // DN_PERFORMANCE branches out here, BEFORE the NF_LOAD-specific target-UPF
    // resolution below: clause 6.14.1's filter set does not require an NF
    // Instance ID, and the upfId in the output (Table 6.14.3-1 "Serving anchor
    // UPF info") is resolved by the NWDAF itself from the NRF. Everything
    // below this block is the untouched NF_LOAD path.
    if (m_analytics_id == "DN_PERFORMANCE") {
      bool dnperf_reachable = false;
      poll_dn_performance(api_root, dnperf_reachable);
      if (!dnperf_reachable) {
        // Same recovery as the NF_LOAD path: only an unreachable NWDAF drops
        // the cached apiRoot. A 204 keeps it, and keeps the preference.
        std::lock_guard<std::mutex> lock(m_mutex);
        m_cached_api_root.clear();
      }
      // Resolve the preference against each session's OWN PCF authorization.
      evaluate_sessions();
      std::this_thread::sleep_for(std::chrono::seconds(m_poll_sec));
      continue;
    }

    // TS 23.288 clause 6.2.2.4 permits discovery "on a periodic basis". The
    // target UPF is re-resolved whenever it is unknown, so a UPF restart (which
    // changes its nfInstanceId in OAI) is picked up without operator action.
    std::string target = {};
    {
      std::lock_guard<std::mutex> lock(m_mutex);
      target = m_target_upf_id_resolved;
    }
    if (target.empty()) {
      std::string discovered = {};
      if (!discover_upf_nf_instance_id(discovered)) {
        Logger::smf_app().warn(
            "NWDAF consumer: no target UPF NF Instance ID could be resolved "
            "from the NRF; skipping this poll. The clause 6.5.1 Analytics "
            "Filter is NOT sent without one - requesting NF load with no "
            "filter would accept another NF's reading.");
        std::this_thread::sleep_for(std::chrono::seconds(m_poll_sec));
        continue;
      }
      {
        std::lock_guard<std::mutex> lock(m_mutex);
        m_target_upf_id_resolved = discovered;
      }
      target = discovered;
      Logger::smf_app().info(
          "NWDAF consumer: target UPF NF Instance ID resolved from the NRF: %s",
          target.c_str());
    }

    std::vector<nwdaf_nf_load_t> loads = {};
    bool nwdaf_reachable               = false;
    if (!request_nf_load(api_root, target, loads, nwdaf_reachable)) {
      if (!nwdaf_reachable) {
        // The NWDAF could not be reached: drop the cached apiRoot so the next
        // iteration rediscovers, and drop the resolved UPF id in case the whole
        // deployment moved.
        std::lock_guard<std::mutex> lock(m_mutex);
        m_cached_api_root.clear();
        if (!m_target_upf_id_is_static) m_target_upf_id_resolved.clear();
      }
      // Reachable-but-no-data (HTTP 204) keeps the apiRoot AND the preference.
      std::this_thread::sleep_for(std::chrono::seconds(m_poll_sec));
      continue;
    }

    for (const auto& load : loads) {
      // clause 6.5.1: when a list of NF Instance IDs is supplied the NWDAF
      // provides analytics per designated instance, so filter on the one this
      // SMF asked about rather than acting on whatever arrives.
      if (!load.nf_instance_id.empty() && load.nf_instance_id != target) {
        // The NWDAF answered about a different NF than the one asked for.
        // Never act on it - acting would mean steering this SMF's sessions on
        // the load of some other NF.
        Logger::smf_app().warn(
            "NWDAF NF_LOAD: reading is for NF instance %s but this SMF asked "
            "about %s - ignoring. If this persists with a STATIC "
            "SMF_NWDAF_TARGET_UPF_NF_INSTANCE_ID, unset it so the identity is "
            "resolved from the NRF: the OAI UPF generates a NEW nfInstanceId "
            "on every restart.",
            load.nf_instance_id.c_str(), target.c_str());
        continue;
      }

      std::string dnai   = {};
      std::string reason = {};
      const bool decided = decide(load, dnai, reason);

      Logger::smf_app().info(
          "NWDAF NF_LOAD analytics: nfInstanceId=%s nfLoadLevelAverage=%d "
          "nfLoadLevelpeak=%d nfCpuUsage=%d nfMemoryUsage=%d confidence=%s",
          load.nf_instance_id.c_str(), load.nf_load_level_average,
          load.nf_load_level_peak, load.nf_cpu_usage, load.nf_memory_usage,
          load.has_confidence ? std::to_string(load.confidence).c_str()
                              : "not provided");

      if (decided) {
        std::string previous = {};
        {
          std::lock_guard<std::mutex> lock(m_mutex);
          previous           = m_preferred_dnai;
          m_preferred_dnai = dnai;
        }
        if (previous != dnai) {
          Logger::smf_app().info(
              "NWDAF analytics preference: %s -> %s (%s). This is a PREFERENCE "
              "only - it is applied per session and only if the PCF authorized "
              "it.",
              previous.empty() ? "<none>" : previous.c_str(), dnai.c_str(),
              reason.c_str());
        } else {
          Logger::smf_app().debug(
              "NWDAF analytics preference unchanged (%s): %s", dnai.c_str(),
              reason.c_str());
        }
      } else {
        Logger::smf_app().debug("NWDAF analytics preference: %s",
                                reason.c_str());
      }
    }

    // Resolve the preference against each session's OWN PCF authorization.
    evaluate_sessions();

    std::this_thread::sleep_for(std::chrono::seconds(m_poll_sec));
  }
}

}  // namespace oai::smf
