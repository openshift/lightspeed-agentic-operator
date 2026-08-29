# EU AI Act — Regulation (EU) 2024/1689

Reference outline of [Regulation (EU) 2024/1689](https://eur-lex.europa.eu/legal-content/EN/TXT/HTML/?uri=OJ:L_202401689) (the EU Artificial Intelligence Act) and its implications for LightSpeed.

## Overview

The EU AI Act is the first comprehensive legal framework for artificial intelligence. It entered into force on **1 August 2024** and introduces a risk-based classification system with obligations that scale with the level of risk an AI system poses to health, safety, and fundamental rights.

It is a directly applicable EU regulation (like GDPR) — no national transposition required.

**Territorial scope**: Applies to any provider placing AI systems on the EU market and to third-country providers whose AI system outputs are used in the EU.

## Risk-Based Classification

The Act classifies AI systems into four tiers:

| Tier | Treatment | Examples |
|------|-----------|----------|
| **Unacceptable risk** | Prohibited outright | Social scoring, manipulative subliminal techniques, untargeted facial-recognition scraping, emotion inference in workplaces/schools |
| **High risk** | Allowed after conformity assessment + CE marking | Critical infrastructure safety, employment/recruitment, credit scoring, law enforcement, migration/border control, education access |
| **Limited risk** | Transparency obligations | Chatbots, deepfake generators, emotion recognition, biometric categorisation |
| **Minimal risk** | No binding rules; voluntary codes encouraged | Spam filters, AI-enabled video games |

## Prohibited Practices (Article 5)

Banned outright (enforceable since **2 February 2025**):

- Subliminal, manipulative, or deceptive techniques causing significant harm
- Exploitation of vulnerabilities (age, disability, socio-economic status)
- Social scoring by public authorities
- Predictive policing based solely on profiling or personality traits
- Untargeted scraping to build facial recognition databases
- Emotion inference in workplaces and schools (except medical/safety)
- Biometric categorisation inferring sensitive attributes (race, political opinion, etc.)
- Real-time remote biometric identification in public spaces (narrow law-enforcement exceptions)

## High-Risk AI Systems (Chapter III, Articles 6–49)

### Classification (Articles 6–7, Annex III)

A system is high-risk if it is:
1. A safety component of a product covered by EU harmonisation legislation requiring third-party conformity assessment, **or**
2. Listed in **Annex III** use-case areas (biometrics, critical infrastructure, education, employment, essential services, law enforcement, migration, justice/democracy)

Exception: systems performing narrow procedural tasks or improving prior human activities may be exempt even if listed in Annex III.

### Provider Requirements (Articles 8–17)

| Requirement | Article | Summary |
|-------------|---------|---------|
| Risk management | Art. 9 | Continuous lifecycle risk management system |
| Data governance | Art. 10 | Training/validation/test data must be relevant, representative, free of errors |
| Technical documentation | Art. 11 | Demonstrate compliance before market placement (Annex IV details) |
| **Record-keeping** | **Art. 12** | **Automatic logging of events over system lifetime** |
| Transparency | Art. 13 | Clear information to deployers on capabilities, limitations, intended use |
| Human oversight | Art. 14 | Design for effective human oversight; humans must be able to intervene |
| Accuracy & robustness | Art. 15 | Appropriate accuracy, robustness, and cybersecurity levels |
| Quality management | Art. 17 | Documented QMS covering all above requirements |

### Deployer Obligations (Article 26)

Deployers (operators using the AI system) must:
- Use the system in accordance with instructions for use
- Ensure human oversight by competent persons
- Monitor operation and report serious incidents
- Conduct fundamental rights impact assessments (public bodies, certain private deployers)
- Keep automatically generated logs for **at least 6 months**
- Ensure AI literacy of relevant staff

### Record-Keeping Detail (Article 12)

Article 12 is directly relevant to LightSpeed audit logging. Key requirements:

**Art. 12(1)**: High-risk AI systems shall technically allow for the **automatic recording of events (logs)** over the lifetime of the system.

**Art. 12(2)**: Logging must enable recording events relevant for:
- (a) Identifying situations that may result in risk or substantial modification
- (b) Facilitating post-market monitoring (Art. 72)
- (c) Monitoring operation by deployers (Art. 26(5))

**Art. 12(3)**: For biometric identification systems (Annex III, point 1(a)), logs must include at minimum:
- Period of each use (start/end timestamps)
- Reference database used
- Input data that led to matches
- Identity of persons verifying results

**What this means for logging implementations**:
- Logging must be automatic (built into the system, not manual)
- Standard application logs alone are insufficient — tamper-evidence is expected
- No prescribed log format yet, but draft standards are in development (prEN 18229-1, ISO/IEC DIS 24970)
- Logs must be retained at least 6 months; technical documentation for 10 years after withdrawal

**Penalties for non-compliance with record-keeping**: Up to EUR 15 million or 3% of global annual turnover (Art. 99, second tier).

### Logs vs Distributed Traces — What Article 12 Actually Requires

A common question is whether Article 12 mandates traditional log files or whether distributed traces (e.g., OpenTelemetry spans) satisfy the requirement.

#### What the regulation says

Article 12(1) uses the phrase:

> High-risk AI systems shall technically allow for the **automatic recording of events (logs)** over the lifetime of the system.

The word "logs" appears only in parentheses — as a clarifying gloss on "automatic recording of events", not as a prescribed technology. Article 12(2) reinforces this by describing the purpose in functional terms:

> logging capabilities shall enable the recording of events relevant for [...] **traceability of the functioning** of a high-risk AI system

The full regulation text (all 113 articles) contains no mention of "log file", "log format", "distributed tracing", "traces", or "spans". It does not prescribe any specific recording technology.

#### Technical standards are delegated, not prescribed

The Act deliberately avoids specifying how records must be kept. Instead, it delegates technical details to:

- **Harmonised standards** (Art. 40): Published by European standardisation bodies (CEN/CENELEC). Compliance creates a presumption of conformity.
- **Common specifications** (Art. 41): Adopted by the Commission as implementing acts when harmonised standards are unavailable or insufficient.

As of July 2026, no harmonised standard or common specification has been published for Article 12. Draft standards exist (prEN 18229-1, ISO/IEC DIS 24970) but are not yet finalised.

Until standards are published, providers must demonstrate that their technical solution meets the functional requirements of Article 12 to a level "at least equivalent" to any common specification (Art. 41(5)).

#### Can distributed traces satisfy Article 12?

Yes — provided they meet Article 12's functional requirements:

| Requirement | Traditional logs | Distributed traces (OTEL spans) |
|-------------|-----------------|----------------------------------|
| **Automatic recording** (Art. 12(1)) | Yes (built-in to system) | Yes (SDK instrumentation) |
| **Event recording** (Art. 12(1)) | Yes (timestamped entries) | Yes (spans with timestamps, events, attributes) |
| **Traceability of functioning** (Art. 12(2)) | Partial (flat, correlation by text parsing) | Strong (parent-child hierarchy, trace IDs, causal ordering) |
| **Identifying risk situations** (Art. 12(2)(a)) | Yes (if events are logged) | Yes (if events are recorded as spans/span events) |
| **Post-market monitoring** (Art. 12(2)(b)) | Yes (searchable entries) | Yes (queryable by trace/span attributes) |
| **Deployer monitoring** (Art. 12(2)(c)) | Yes (stdout/file access) | Yes (trace backends with UI, dashboards) |
| **Lifetime recording** (Art. 12(1)) | Depends on retention config | Depends on retention config |

Distributed traces are arguably **better suited** than flat logs for the "traceability of the functioning" requirement (Art. 12(2)) because they natively capture:
- **Causal ordering** — parent-child span relationships show exactly which action triggered which consequence
- **Duration** — span start/end times measure how long each operation took
- **Cross-service correlation** — trace context propagation links operator decisions to sandbox agent execution

#### Where traces alone fall short

Traces are not a complete solution. Key gaps:

1. **Completeness** — OpenTelemetry spans in the sandbox only cover tool execution. Thinking, text output, agent start/complete events are not recorded as spans (see [audit-logging-tracing.md](./audit-logging-tracing.md) for the full event coverage matrix).
2. **Full payloads** — Span attributes are truncated (input: 300 chars, output: 500 chars). Article 12 requires recording events "over the lifetime of the system" — truncated data may not satisfy auditors who need complete tool inputs/outputs.
3. **Tamper-evidence** — OpenTelemetry provides no built-in integrity guarantees. TLS protects data in transit, but once stored, records can be modified without detection. Article 12 implies evidentiary value, and conformity assessors will likely expect some form of integrity protection. Practical approaches: immutable storage (e.g., S3 Object Lock, WORM-compliant backends), hash chains at the SDK level, or Collector-level integrity processing.
4. **Retention** — Trace backends (Tempo, Jaeger) typically retain data for days to weeks, not the 6-month minimum required by Article 26(6) for deployers. Long-term retention requires explicit configuration or export to archival storage.

#### Recommended approach — dual-signal with unified trace context

The strongest compliance posture combines both signals, linked by trace context:

| Signal | Role | Retention |
|--------|------|-----------|
| **OTEL spans** | Traceability, causal ordering, duration, cross-service correlation | Short/medium-term (trace backend) |
| **JSON audit logs** | Complete event record, full payloads, tamper-evident storage | Long-term (≥6 months, immutable storage) |
| **Trace context** (`trace_id`) | Links both signals for a single proposal lifecycle | Embedded in both |

LightSpeed already partially implements this pattern:
- The operator emits both JSON audit logs and OTEL span events for each lifecycle event ([audit.go](https://github.com/openshift/lightspeed-agentic-operator/blob/main/controller/proposal/audit.go))
- The sandbox emits JSON audit logs with `trace_id` fields alongside OTEL tool spans ([audit.py](https://github.com/openshift/lightspeed-agentic-sandbox/blob/main/src/lightspeed_agentic/audit.py))
- Both signals share the same `trace_id`, enabling correlation

Remaining gaps to close for full Article 12 compliance:
1. Add tamper-evidence to audit log storage (immutable backend or hash chains)
2. Expand span coverage to include all audit events (thinking, text, start/complete)
3. Configure trace backend retention to meet the 6-month minimum, or export spans to archival storage
4. Remove or increase span attribute truncation limits for compliance-relevant attributes

### Industry Approaches to Article 12 Compliance

No harmonised standard exists yet, so companies are converging on a few patterns. The table below summarises the main approaches observed in the ecosystem as of mid-2026.

#### Observability platforms — traces as the primary signal

Traditional APM vendors are positioning distributed tracing as a foundation for Article 12 compliance:

- **[New Relic](https://newrelic.com/blog/ai/the-eu-artificial-intelligence-act-and-observability)** — positions its observability platform as an Article 12 compliance tool. Supports OpenTelemetry ingestion, log forwarding (Fluent Bit, Fluentd, Logstash), and AI-specific telemetry (token counts, model identifiers, latency). Offers Scorecards for compliance tracking and shadow AI detection. Does not provide tamper-evidence or structured compliance audit formats natively.

- **[Datadog](https://www.datadoghq.com/products/ai/agent-observability/)** — Agent Observability captures end-to-end traces across retrieval, prompt assembly, model invocation, and tool calls. Includes sensitive data scanning/redaction and role-based access control. Traces are scored automatically, with alerts for quality issues. Focuses on operational observability rather than regulatory audit format.

- **[Dynatrace](https://www.dynatrace.com/solutions/ai-observability/)** — provides end-to-end tracing from prompt to response, guardrail monitoring, hallucination detection, and compliance dashboards. Documents all inputs and outputs with full data lineage. Like other APM tools, primarily designed for operational monitoring rather than producing regulator-ready audit evidence.

These platforms capture traces and metrics well, but as [noted by multiple analysts](https://www.helpnetsecurity.com/2026/04/16/eu-ai-act-logging-requirements/), no general-purpose observability or SIEM tool today produces output in the format the AI Office has signalled as adequate. They log HTTP-level activity but do not capture per-agent decisions or chain integrity in a regulator-presentable structure.

#### LLM-native observability — traces + evaluations

Platforms built specifically for LLM/agent workflows provide deeper AI-specific tracing:

- **[LangSmith](https://www.langchain.com/blog/langsmith-langchain-oss-eu-ai-act)** (LangChain) — end-to-end tracing of every LLM call, tool invocation, and reasoning step with structured metadata. Production evaluations score traces for bias, hallucination, toxicity. Human oversight via annotation queues. Extended trace retention up to 400 days (managed cloud) or unlimited (self-hosted). EU data residency option. LangChain has an [active feature request](https://github.com/langchain-ai/langchain/issues/35357) for structured compliance audit logging with tamper-evident formats, acknowledging that current tracing is designed for debugging/monitoring, not regulatory audit.

- **[Langfuse](https://langfuse.com/)** (open source, MIT) — self-hostable LLM observability with tracing, evaluations, and prompt management. Traces capture LLM calls, tool executions, and agent orchestration steps. 2,300+ companies, billions of observations/month. Data sovereignty via self-hosting. [Positioned for EU AI Act compliance](https://wz-it.com/en/blog/langfuse-llm-observability-eu-ai-act-logging/) with configurable retention and full data control, but does not provide tamper-evidence natively.

Both platforms use **traces (not traditional logs)** as their primary compliance signal, with structured spans capturing inputs, outputs, model parameters, token usage, and timing.

#### Gateway-level audit writing — structured logs with integrity

An emerging architectural pattern moves audit writing out of the application into a gateway layer:

- **[DeepInspect](https://www.deepinspect.ai/blog/guides-eu-ai-act-article-12-logging-implementation)** — advocates gateway-level audit writing rather than application-level logging. The gateway sits between the caller and the LLM endpoint, writing structured audit records from a position the application cannot modify. Uses HMAC-based chain verification: each record's signature is an HMAC of the prior record's signature plus the current content. A regulator who replays the chain can detect any altered byte. Identity resolution at TLS boundary, payload classification, per-route policy, six-month retention with sector-law extensions. Claims <50ms per decision.

- **[Asqav](https://www.asqav.com/blog/posts/eu-ai-act-audit-trail-requirements)** — cryptographic signing for agent logs using ML-DSA (NIST FIPS 204 post-quantum signatures). Each agent action is signed with a key the agent doesn't hold, chained to the previous signature. Receipts stored outside the agent's trust boundary. Cold archive defaults to 7-year retention. Creates tamper-evident records targeting Articles 12, 19, and 26.

This pattern addresses the three structural failure modes of application-level logging identified by DeepInspect: selective logging (missing edge-case failures), suppression (application can rewrite its own logs), and loss on crash (action taken but audit record not committed).

#### Compliance-specific platforms — audit-grade evidence

- **[Agent Audit](https://www.agentaudit.co.uk/solutions/eu-ai-act/)** — captures events at the SDK boundary in real time. Decision-drift detection and material event surfacing built for Article 12. Produces tamper-evident records of every decision. Cold archive defaults to 7-year retention.

- **[CertifiedData](https://certifieddata.io/eu-ai-act/article-12-record-keeping)** — connects signed AI decision records, certified artifact references, and public verification keys into a verifiable evidence layer.

- **[TrueScreen](https://truescreen.io/insights/ai-act-record-keeping-requirements/)** — automated certification workflows integrated with existing AI logging infrastructure, producing tamper-proof audit trails with eIDAS-compliant qualified timestamps.

#### Cloud platform approaches — infrastructure-level compliance

- **[Microsoft Azure](https://www.microsoft.com/en-us/trust-center/compliance/eu-ai-act)** — Azure AI Foundry for model cards and transparency notes, Diagnostic Settings and tracing to App Insights and Log Analytics, Defender for Cloud with EU AI Act regulatory standard for compliance reporting, Purview for audit log retention. ISO/IEC 42001:2023 certified. Does not provide Article 12-specific tamper-evident logging natively — relies on platform access controls and retention policies.

- **[Databricks](https://www.confident-ai.com/knowledge-base/compare/best-ai-observability-tools-2026)** — Unity Catalog and Lakehouse Monitoring for data lineage and AI system monitoring. Positioned for compliance through existing data governance infrastructure.

#### Summary of approaches

| Approach | Primary signal | Tamper-evidence | Retention | Example |
|----------|---------------|-----------------|-----------|---------|
| APM/observability platforms | Distributed traces (OTEL) | No (access controls only) | Configurable, often short | New Relic, Datadog, Dynatrace |
| LLM-native observability | Structured traces | No (evolving) | Up to 400 days or unlimited (self-hosted) | LangSmith, Langfuse |
| Gateway-level audit | Structured logs with HMAC chains | Yes (hash chains, digital signatures) | 6 months–7 years | DeepInspect, Asqav |
| Compliance platforms | SDK-boundary event capture | Yes (tamper-evident by design) | 7+ years | Agent Audit, CertifiedData, TrueScreen |
| Cloud platforms | Platform logs + diagnostics | Partial (platform access controls) | Configurable | Azure AI, Databricks |

#### Key takeaways

1. **No one uses traditional log files alone.** Every serious implementation uses either distributed traces, structured audit records, or both.
2. **Traces are necessary but not sufficient.** Observability platforms provide tracing but lack tamper-evidence and regulator-ready formats. Most are adding compliance features but acknowledge the gap.
3. **Tamper-evidence is the differentiator.** The companies furthest ahead (Asqav, DeepInspect, Agent Audit) all implement cryptographic integrity — hash chains, digital signatures, or both — even though Article 12 does not explicitly mandate it. The reasoning: logs without integrity have zero evidentiary value if challenged.
4. **Gateway-level writing is emerging as best practice.** Moving audit writing out of the application (where it can be suppressed or selectively omitted) into an independent gateway or SDK boundary layer addresses structural reliability problems.
5. **The dual-signal pattern (traces + audit logs) is the most common.** LangSmith, Langfuse, and APM platforms all capture traces; organisations then layer structured audit logs or compliance-specific evidence on top. LightSpeed's existing architecture (OTEL spans + JSON audit logs linked by `trace_id`) aligns with this industry consensus.

## Transparency Obligations (Article 50)

Limited-risk systems must ensure users know they are interacting with AI:

- **AI-generated content**: Must be marked as artificially generated or manipulated (deepfakes)
- **Direct interaction**: Systems interacting with persons must disclose they are AI (chatbots)
- **Emotion recognition / biometric categorisation**: Must inform subjects and process personal data per GDPR

## General-Purpose AI Models (Chapter V, Articles 51–56)

### All GPAI Providers (Article 53)

- Maintain and provide technical documentation
- Provide information and documentation to downstream providers integrating the model
- Establish copyright compliance policy (Copyright Directive)
- Publish sufficiently detailed training content summary

### Systemic Risk Models (Article 55)

Models trained with >10^25 FLOPs or designated by the Commission face additional obligations:

- Perform model evaluations including adversarial testing
- Assess and mitigate systemic risks
- Track, document, and report serious incidents to the AI Office
- Ensure adequate cybersecurity protections

Open-source models have reduced obligations unless they present systemic risk.

### Codes of Practice (Article 56)

The AI Office facilitates codes of practice for GPAI providers. Compliance with an approved code creates a presumption of conformity.

## Governance (Chapter VII, Articles 63–71)

| Body | Role |
|------|------|
| **AI Office** (Art. 63–64) | Commission body for implementation, monitoring, and enforcement of GPAI obligations |
| **European AI Board** (Art. 65–66) | Advisory body of Member State representatives; coordinates national authorities |
| **Advisory Forum** (Art. 67) | Stakeholder input (industry, civil society, academia) |
| **Scientific Panel** (Art. 68) | Independent experts supporting enforcement, especially for systemic risk |
| **National competent authorities** (Art. 70) | Market surveillance and enforcement at Member State level |
| **EU Database** (Art. 71) | Public registry of high-risk AI systems listed in Annex III |

## Penalties (Articles 99–101)

| Violation | Maximum Fine |
|-----------|-------------|
| Prohibited practices (Art. 5) | EUR 35 million or **7%** of global annual turnover |
| High-risk requirements (Chapter III), GPAI obligations (Chapter V), other key provisions | EUR 15 million or **3%** of global annual turnover |
| Supplying incorrect information to authorities | EUR 7.5 million or **1%** of global annual turnover |

SMEs and startups pay the lower of the two amounts in each tier. EU institutions are subject to fines up to EUR 1.5 million (Art. 100).

## Implementation Timeline

| Date | Milestone |
|------|-----------|
| **1 Aug 2024** | Regulation entered into force |
| **2 Feb 2025** | Prohibited practices ban + AI literacy obligations take effect |
| **2 Aug 2025** | GPAI model obligations, governance structure, codes of practice, penalty framework |
| **2 Aug 2026** | Full applicability for high-risk AI systems (Annex III): conformity assessment, CE marking, EU database registration, record-keeping, quality management |
| **2 Aug 2027** | High-risk AI embedded in products under Annex I harmonisation legislation |

## Relevance to LightSpeed

### Risk classification

LightSpeed is an agentic AI system that autonomously executes proposals on OpenShift clusters (creating/modifying Kubernetes resources, running commands). Depending on deployment context, it could fall under high-risk classification:
- **Critical infrastructure** (Annex III, Area 2): If managing safety components of digital infrastructure
- **Employment** (Annex III, Area 4): If used for workforce-related task allocation or monitoring
- **Essential services** (Annex III, Area 5): If decisions affect access to essential public/private services

The exact classification depends on the specific deployment use case.

### Applicable requirements

If classified as high-risk, the following requirements directly map to existing LightSpeed capabilities:

| EU AI Act Requirement | LightSpeed Capability | Gap |
|-----------------------|-----------------------|-----|
| Art. 12 — Automatic event logging | Audit logging (JSON to stdout) + OTEL span events | Log tamper-evidence, retention guarantees |
| Art. 13 — Transparency to deployers | Proposal lifecycle phases, AnalysisResult CRs | Formal instructions-for-use documentation |
| Art. 14 — Human oversight | Approval gates (Proposed → Executing requires human approval) | Documenting override/intervention mechanisms |
| Art. 26(5) — Deployer monitoring | OTEL tracing export, audit logs | Deployer-facing monitoring documentation |
| Art. 26(6) — Log retention ≥6 months | Configurable via CRD, logs to stdout | No built-in retention enforcement |

### Key gaps to address

1. **Tamper-evident logging** — Current JSON-to-stdout audit logs have no integrity guarantees. Article 12 implies logs must have evidentiary value.
2. **Log retention** — No built-in mechanism to enforce the 6-month minimum. Depends on cluster log aggregation.
3. **Formal documentation** — Technical documentation (Art. 11, Annex IV) and instructions for use (Art. 13) need structured authoring.
4. **Conformity assessment** — If high-risk, requires CE marking and either internal control (Annex VI) or third-party assessment (Annex VII).
5. **EU database registration** — High-risk systems under Annex III must be registered (Art. 49).

## Structure of the Regulation

The Act contains **13 chapters**, **113 articles**, and **13 annexes**. Full article-level structure:

<details>
<summary>Complete chapter and article listing</summary>

### Chapter I — General Provisions (Articles 1–4)
- Art. 1: Subject matter
- Art. 2: Scope
- Art. 3: Definitions (68 defined terms)
- Art. 4: AI literacy

### Chapter II — Prohibited AI Practices (Article 5)
- Art. 5: Prohibited AI practices

### Chapter III — High-Risk AI Systems (Articles 6–49)

**Section 1 — Classification**
- Art. 6: Classification rules
- Art. 7: Amendments to Annex III

**Section 2 — Requirements**
- Art. 8: Compliance with requirements
- Art. 9: Risk management system
- Art. 10: Data and data governance
- Art. 11: Technical documentation
- Art. 12: Record-keeping
- Art. 13: Transparency and provision of information to deployers
- Art. 14: Human oversight
- Art. 15: Accuracy, robustness and cybersecurity

**Section 3 — Obligations of Providers and Deployers**
- Art. 16–27: Provider, deployer, importer, distributor obligations; value chain responsibilities; fundamental rights impact assessments

**Section 4 — Notifying Authorities and Notified Bodies**
- Art. 28–39: Notification procedures, notified body requirements, coordination

**Section 5 — Standards, Conformity Assessment, Certificates, Registration**
- Art. 40–49: Harmonised standards, common specifications, conformity assessment procedures, CE marking, EU declaration of conformity, registration

### Chapter IV — Transparency Obligations (Article 50)
- Art. 50: Transparency obligations for certain AI systems

### Chapter V — General-Purpose AI Models (Articles 51–56)
- Art. 51–52: Classification of GPAI models, including systemic risk designation
- Art. 53–54: Obligations for all GPAI providers
- Art. 55: Additional obligations for systemic risk models
- Art. 56: Codes of practice

### Chapter VI — Measures in Support of Innovation (Articles 57–62)
- Art. 57–59: AI regulatory sandboxes
- Art. 60–61: Real-world testing
- Art. 62: SME and startup support measures

### Chapter VII — Governance (Articles 63–71)
- Art. 63–64: AI Office
- Art. 65–66: European AI Board
- Art. 67: Advisory Forum
- Art. 68–69: Scientific Panel
- Art. 70: National competent authorities
- Art. 71: EU database for high-risk AI systems

### Chapter IX — Post-Market Monitoring, Information Sharing, Market Surveillance (Articles 72–94)
- Art. 72: Post-market monitoring
- Art. 73: Serious incident reporting
- Art. 74–84: Market surveillance, enforcement procedures, safeguard procedures
- Art. 85–87: Remedies (complaints, right to explanation, whistleblower protection)
- Art. 88–94: GPAI model enforcement (monitoring, evaluations, measures, procedural rights)

### Chapter X — Codes of Conduct and Guidelines (Articles 95–96)

### Chapter XI — Delegation of Power and Committee Procedure (Articles 97–98)

### Chapter XII — Penalties (Articles 99–101)

### Chapter XIII — Final Provisions (Articles 102–113)
- Art. 102–110: Amendments to other EU legislation
- Art. 111: Transitional provisions for existing AI systems
- Art. 112: Evaluation and review
- Art. 113: Entry into force and application dates

### Annexes
| Annex | Content |
|-------|---------|
| I | Union harmonisation legislation (product safety directives) |
| II | Criminal offences referenced in Art. 5(1) |
| III | High-risk AI system use-case areas |
| IV | Technical documentation requirements |
| V | EU declaration of conformity template |
| VI | Conformity assessment — internal control procedure |
| VII | Conformity assessment — QMS + technical documentation |
| VIII | Registration information for high-risk AI systems |
| IX | Registration information for real-world testing |
| X | EU large-scale IT systems legislation |
| XI | Technical documentation for GPAI models |
| XII | Transparency information for GPAI models |
| XIII | Criteria for systemic risk designation |

</details>

## Key Definitions (Article 3, selected)

| # | Term | Definition |
|---|------|------------|
| 1 | AI system | Machine-based system operating with varying levels of autonomy, generating outputs (predictions, recommendations, decisions) that influence environments |
| 3 | Provider | Entity that develops an AI system or GPAI model and places it on the market or puts it into service |
| 4 | Deployer | Person or entity using an AI system under its authority (except personal non-professional use) |
| 8 | Operator | Collective term: provider, product manufacturer, deployer, authorised representative, importer, or distributor |
| 12 | Intended purpose | Use intended by the provider, including specific context and conditions |
| 23 | Substantial modification | Post-market change affecting compliance or intended purpose |
| 49 | Serious incident | Incident directly or indirectly leading to death, serious harm, infrastructure disruption, or fundamental rights infringement |
| 56 | AI literacy | Skills, knowledge, and understanding enabling informed deployment and awareness of AI risks/opportunities |
| 63 | General-purpose AI model | Model displaying significant generality, capable of competently performing a wide range of distinct tasks |
| 65 | Systemic risk | Risk specific to high-impact capabilities of GPAI models, with significant impact on the Union market |

## Sources

- [Official text — EUR-Lex](https://eur-lex.europa.eu/legal-content/EN/TXT/HTML/?uri=OJ:L_202401689)
- [AI Act Explorer](https://artificialintelligenceact.eu/ai-act-explorer/)
- [High-level summary](https://artificialintelligenceact.eu/high-level-summary/)
- [Article 12 — Record-keeping](https://artificialintelligenceact.eu/article/12/)
- [Article 12 logging requirements for AI agents](https://www.helpnetsecurity.com/2026/04/16/eu-ai-act-logging-requirements/)
- [EU AI Act summary — GDPR Local](https://gdprlocal.com/eu-ai-act-summary/)
- [AI Act complete guide — euaiactguide.com](https://euaiactguide.com/eu-ai-act-summary-2026/)
