#!/usr/bin/env bash
#
# write_spyglass_report: emit a Spyglass "custom link" HTML report for an e2e
# run, so a reviewer opening the Prow job lands on a page with the test verdict,
# the parsed failure messages, and links straight to the JUnit and the build
# log -- rather than scrolling a two-hour build-log.txt to find them.
#
# Sourced by hack/ci-e2e-<cloud>.sh and called once, after the test verdict is
# known:
#
#   source "${here}/lib/spyglass-report.sh"
#   write_spyglass_report aws "${test_rc}"
#
# The file it writes, ${ARTIFACT_DIR}/custom-link-<cloud>.html, is picked up by
# Deck's html lens: openshift/release core-services/prow/02_config/_config.yaml
# lists a required_files pattern of .*/custom-link-.*\.html. No release-repo
# change is needed to surface it.
#
# The report is best-effort and self-contained: it never fails the job (callers
# guard it), reads only what the suite already wrote to ${ARTIFACT_DIR}, and is
# a no-op when ARTIFACT_DIR is unset -- so it does nothing on a developer's own
# `make test-e2e-*` run.
#
# All three clouds share this one function; the cloud is a label, so gcp works
# the moment hack/ci-e2e-gcp.sh exists and calls `write_spyglass_report gcp`.

# write_spyglass_report <cloud> <exit_code>
#   cloud       one of aws | azure | gcp (used in the title and the filename)
#   exit_code   the test's exit status; 0 renders PASSED, anything else FAILED
write_spyglass_report() {
  local cloud="${1:?write_spyglass_report: cloud (aws|azure|gcp) required}"
  local exit_code="${2:-0}"
  # The ci-operator step that runs hack/ci-e2e-<cloud>.sh is named "test" in
  # every bgp-cloud-connector e2e test (e2e-aws, e2e-aws-operator, e2e-azure-
  # operator, e2e-gcp-operator, e2e-rosa-operator), so artifacts land under
  # .../<test>/test/. Override with SPYGLASS_STEP_NAME if that ever changes.
  local step_name="${SPYGLASS_STEP_NAME:-test}"
  local job_safe="${JOB_NAME_SAFE:-${JOB_NAME:-unknown}}"
  local gcs_job_path=""
  # ci-operator uploads to the private test-platform-results bucket, which
  # unauthenticated browsers cannot read; the censored public mirror
  # test-platform-results-public is what humans reach from Spyglass. Override
  # via GCS_PUBLIC_BUCKET if the mirror name changes again.
  local gcs_bucket="${GCS_PUBLIC_BUCKET:-test-platform-results-public}"
  local gcsweb_base="https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/${gcs_bucket}"
  local artifacts_base=""
  local step_base=""
  local report=""
  local status_label="PASSED"
  local status_color="#81c784"
  local junit_count=0
  local report_count=0
  local write_rc=0
  local reports_tmp="" failures_tmp="" summary_tmp="" skipped_tmp=""
  local total_tests=0 total_fails=0 total_skips=0 total_errs=0 total_time=0
  local tline="" discard=""
  local enc="" href="" label="" fname="" fmsg="" sname=""

  # HTML-escape a string for safe use in text nodes or attribute values.
  # Use sed: bash ${var//\"/&quot;} treats & as the matched text.
  html_escape() {
    printf '%s' "${1-}" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g' -e "s/'/\&#39;/g"
  }

  # Percent-encode one path segment (keeps unreserved RFC 3986 chars).
  urlencode_component() {
    local LC_ALL=C
    local s="${1-}" i c out=""
    for (( i = 0; i < ${#s}; i++ )); do
      c="${s:i:1}"
      case "${c}" in
        [a-zA-Z0-9.~_-]) out+="${c}" ;;
        *) printf -v out '%s%%%02X' "${out}" "'${c}" ;;
      esac
    done
    printf '%s' "${out}"
  }

  # Percent-encode each path component of a relative artifact path; preserve '/'.
  urlencode_path() {
    local path="${1-}" result="" part first=1
    while [[ "${path}" == *"/"* ]]; do
      part="${path%%/*}"
      path="${path#*/}"
      if [[ "${first}" -eq 1 ]]; then
        first=0
      else
        result+="/"
      fi
      result+="$(urlencode_component "${part}")"
    done
    if [[ "${first}" -eq 1 ]]; then
      result="$(urlencode_component "${path}")"
    else
      result+="/$(urlencode_component "${path}")"
    fi
    printf '%s' "${result}"
  }

  if [[ -z "${ARTIFACT_DIR:-}" ]]; then
    echo "====> ARTIFACT_DIR unset; skipping ${cloud} Spyglass report"
    return 0
  fi

  report="${ARTIFACT_DIR}/custom-link-${cloud}.html"

  if [[ "${JOB_TYPE:-}" == "presubmit" && -n "${PULL_NUMBER:-}" ]]; then
    gcs_job_path="pr-logs/pull/${REPO_OWNER:-}_${REPO_NAME:-}/${PULL_NUMBER}/${JOB_NAME:-}/${BUILD_ID:-}"
  else
    gcs_job_path="logs/${JOB_NAME:-}/${BUILD_ID:-}"
  fi
  # ci-operator uploads ARTIFACT_DIR under .../<test>/<step>/artifacts/
  artifacts_base="${gcsweb_base}/${gcs_job_path}/artifacts/${job_safe}/${step_name}/artifacts"
  step_base="${gcsweb_base}/${gcs_job_path}/artifacts/${job_safe}/${step_name}"

  if [[ "${exit_code}" -ne 0 ]]; then
    status_label="FAILED (exit ${exit_code})"
    status_color="#ef5350"
  fi

  # Collect relative paths (portable; avoid mapfile for older bash)
  reports_tmp="$(mktemp)"
  failures_tmp="$(mktemp)"
  summary_tmp="$(mktemp)"
  skipped_tmp="$(mktemp)"

  if [[ -d "${ARTIFACT_DIR}" ]]; then
    find "${ARTIFACT_DIR}" -type f \
      \( -iname '*.xml' -o -iname '*.json' -o -iname '*.log' -o -iname '*.txt' \
         -o -iname '*.yaml' -o -iname '*.yml' -o -iname '*.tar' -o -iname '*.gz' \
         -o -iname '*.tgz' -o -iname '*.out' \) 2>/dev/null \
      | sed "s|^${ARTIFACT_DIR}/||" | sort > "${reports_tmp}" || true
    report_count="$(wc -l < "${reports_tmp}" | tr -d ' ')"
    junit_count="$(find "${ARTIFACT_DIR}" -type f -name '*.xml' 2>/dev/null | wc -l | tr -d ' ')"
  fi

  # Parse the JUnit XML (Ginkgo's reporters.GenerateJUnitReport shape:
  #   <testsuite tests=".." failures=".." skipped=".." errors=".." time="..">
  #   <testcase name=".." status=".."><failure message=".."/></testcase>).
  # No python/jq: awk pairs each <failure>/<skipped> with the <testcase name>
  # that precedes it, and sums the suite totals. One record per line, tab-
  # separated, tagged T (totals) / F (failure) / S (skipped).
  if [[ "${junit_count}" -gt 0 ]]; then
    # shellcheck disable=SC2016
    find "${ARTIFACT_DIR}" -type f -name '*.xml' -print0 2>/dev/null \
      | xargs -0 awk '
          function attr(s, key,   v) {
            if (match(s, key "=\"[^\"]*\"")) {
              v = substr(s, RSTART, RLENGTH)
              sub(key "=\"", "", v); sub(/"$/, "", v)
              return v
            }
            return ""
          }
          /<testsuite[ >]/ {
            tests += attr($0,"tests"); fails += attr($0,"failures")
            skips += attr($0,"skipped"); errs += attr($0,"errors")
            time += attr($0,"time")
          }
          /<testcase[ >]/ { name = attr($0,"name") }
          /<failure[ >]/  { print "F\t" name "\t" attr($0,"message") }
          /<skipped[ />]/ { print "S\t" name }
          END { printf "T\t%d\t%d\t%d\t%d\t%.0f\n", tests, fails, skips, errs, time }
        ' > "${summary_tmp}" 2>/dev/null || true

    tline="$(grep -m1 "^T$(printf '\t')" "${summary_tmp}" 2>/dev/null || true)"
    if [[ -n "${tline}" ]]; then
      IFS=$'\t' read -r discard total_tests total_fails total_skips total_errs total_time <<<"${tline}"
    fi

    # Decode XML entities in the extracted text; it is re-escaped for HTML at
    # render time. Decode &amp; last so a literal &lt; in the source survives.
    grep "^F$(printf '\t')" "${summary_tmp}" 2>/dev/null | cut -f2- \
      | sed -e 's/&lt;/</g' -e 's/&gt;/>/g' -e 's/&quot;/"/g' -e "s/&apos;/'/g" -e 's/&amp;/\&/g' \
      | head -60 > "${failures_tmp}" || true
    grep "^S$(printf '\t')" "${summary_tmp}" 2>/dev/null | cut -f2- \
      | sed -e 's/&lt;/</g' -e 's/&gt;/>/g' -e 's/&quot;/"/g' -e "s/&apos;/'/g" -e 's/&amp;/\&/g' \
      | grep -E '.' | sort -u > "${skipped_tmp}" || true
  fi

  {
    cat <<EOF
<html>
<head>
  <title>BGP Cloud Connector ${cloud} e2e</title>
  <meta name="description" content="Links to ${cloud} e2e logs, JUnit failures, and report artifacts for bgp-cloud-connector.">
  <style>
    body {
      background-color: #303030;
      color: #eee;
      font-family: "Roboto", "Helvetica", "Arial", sans-serif;
      padding: 16px;
      margin: 0;
      font-size: 14px;
    }
    h1 { font-size: 18px; margin: 0 0 8px 0; }
    h2 { font-size: 15px; margin: 18px 0 8px 0; color: #90caf9; }
    .status { color: ${status_color}; font-weight: 700; margin-bottom: 12px; }
    a { color: #4fc3f7; text-decoration: none; }
    a:hover { text-decoration: underline; }
    .btn {
      display: inline-block;
      padding: 6px 14px;
      margin: 4px 8px 4px 0;
      border: 2px solid #4E9AF1;
      border-radius: 1em;
      color: #fff !important;
      background-color: #4E9AF1;
      text-decoration: none !important;
    }
    .btn:hover { border-color: #fff; }
    ul { margin: 6px 0 0 18px; padding: 0; }
    li { margin: 4px 0; word-break: break-all; }
    pre {
      background: #212121;
      border: 1px solid #555;
      padding: 10px;
      overflow-x: auto;
      white-space: pre-wrap;
      max-height: 320px;
    }
    .muted { color: #aaa; font-size: 12px; }
    .empty { color: #999; font-style: italic; }
    .tiles { margin: 4px 0 0 0; }
    .tile {
      display: inline-block;
      padding: 4px 10px;
      margin: 2px 6px 2px 0;
      border-radius: 6px;
      background: #424242;
      font-weight: 700;
    }
    .tile.fail { background: #5c2b2b; color: #ff8a80; }
    .tile.skip { background: #4a4320; color: #ffd54f; }
    .fail { color: #ef5350; font-weight: 700; }
    details { margin-top: 6px; }
    summary { cursor: pointer; color: #90caf9; }
  </style>
</head>
<body>
  <h1>BGP Cloud Connector ${cloud} e2e</h1>
  <div class="status">${status_label}</div>
  <p class="muted">Cloud <code>${cloud}</code> · job <code>$(html_escape "${JOB_NAME:-unknown}")</code> · build <code>$(html_escape "${BUILD_ID:-unknown}")</code></p>

  <h2>Quick links</h2>
  <a class="btn" href="$(html_escape "${step_base}/build-log.txt")" target="_blank">Step build log</a>
  <a class="btn" href="$(html_escape "${artifacts_base}/junit/")" target="_blank">JUnit folder</a>
  <a class="btn" href="$(html_escape "${artifacts_base}/")" target="_blank">Artifacts folder</a>

  <h2>Summary</h2>
EOF

    if [[ "${junit_count}" -gt 0 ]]; then
      echo '  <p class="tiles">'
      echo "    <span class=\"tile\">${total_tests} specs</span>"
      echo "    <span class=\"tile fail\">${total_fails} failed</span>"
      echo "    <span class=\"tile skip\">${total_skips} skipped</span>"
      echo "    <span class=\"tile\">${total_time}s</span>"
      echo '  </p>'
    else
      echo '  <p class="empty">No JUnit report found (check the step build log).</p>'
    fi

    echo "  <h2>Failed specs (${total_fails})</h2>"
    if [[ -s "${failures_tmp}" ]]; then
      echo "  <ul>"
      while IFS=$'\t' read -r fname fmsg; do
        [[ -z "${fname}${fmsg}" ]] && continue
        echo "    <li><span class=\"fail\">&#10007;</span> <b>$(html_escape "${fname}")</b> &mdash; $(html_escape "${fmsg}")</li>"
      done < "${failures_tmp}"
      echo "  </ul>"
    else
      if [[ "${exit_code}" -ne 0 ]]; then
        echo '  <p class="empty">No per-spec failures parsed (a setup/teardown failure? check the step build log).</p>'
      else
        echo '  <p class="empty">No failures recorded.</p>'
      fi
    fi

    if [[ -s "${skipped_tmp}" ]]; then
      echo "  <details><summary>Skipped specs (${total_skips})</summary>"
      echo "  <ul>"
      while IFS= read -r sname; do
        [[ -z "${sname}" ]] && continue
        echo "    <li>$(html_escape "${sname}")</li>"
      done < "${skipped_tmp}"
      echo "  </ul></details>"
    fi

    echo "  <h2>Report artifacts (${report_count})</h2>"
    if [[ "${report_count}" -gt 0 ]]; then
      echo "  <ul>"
      while IFS= read -r rel; do
        [[ -z "${rel}" ]] && continue
        enc="$(urlencode_path "${rel}")"
        href="$(html_escape "${artifacts_base}/${enc}")"
        label="$(html_escape "${rel}")"
        echo "    <li><a href=\"${href}\" target=\"_blank\">${label}</a></li>"
      done < "${reports_tmp}"
      echo "  </ul>"
    else
      echo '  <p class="empty">No report artifacts uploaded.</p>'
    fi

    cat <<EOF
  <p class="muted">Tip: full test output is in the step build log linked above.</p>
</body>
</html>
EOF
  } > "${report}" || write_rc=$?

  if [[ "${write_rc}" -eq 0 ]]; then
    echo "====> Wrote ${cloud} Spyglass report: ${report}"
    echo "====> Spyglass / GCSWEB artifacts base: ${artifacts_base}/"
  fi
  rm -f "${reports_tmp}" "${failures_tmp}" "${summary_tmp}" "${skipped_tmp}"
  return "${write_rc}"
}
