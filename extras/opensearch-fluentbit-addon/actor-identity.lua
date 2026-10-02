-- Derives openchoreo_component / openchoreo_project / openchoreo_environment
-- (names) plus the real openchoreo.dev/*-uid Kubernetes labels for actor log
-- lines, the same way the kubernetes filter derives both from pod labels for
-- a normal component.
--
-- internal/actorlog nests the identity fields under one of two label-group
-- keys depending on environment (internal/actorlog/logger.go LabelsKey):
-- "logging.googleapis.com/labels" on GCE, "labels" elsewhere.
--
-- ate.atespace is the cell namespace, "dp-<cpNs>-<project>-<environment>-<hash8>",
-- readable except for its trailing hash. ate.actor.name is
-- internal/naming.Identity.ActorName: "<component>-<environment>-a-<hash10>"
-- when the operator knows the component/environment names, or the opaque
-- "a-<hash10>" otherwise (actors created before this naming scheme).
-- Splitting both requires matching a known environment name, since
-- cpNs/project/component may themselves contain hyphens and there's no
-- other delimiter. Extend ENVIRONMENTS for a DeploymentPipeline using other
-- names. cpNs and project are assumed to be single hyphen-free tokens.
--
-- Names alone aren't enough for the observer's own log query, which
-- unconditionally filters by openchoreo.dev/namespace (the project's own
-- Kubernetes namespace, a separate value from the project's name even
-- though they're often equal) and, when scoped further, by
-- openchoreo.dev/{component,project,environment}-uid -- real Kubernetes
-- label values, keyed by UID, not name. Those values are published by
-- oc-substrate's ActorTemplate controller into a ConfigMap (one file per
-- entry once mounted, named exactly as built by the read_uid calls below),
-- mounted into this pod at ATTRIBUTION_DIR. A missing file (not yet
-- reconciled, or the component predates the readable-name change) just
-- means that one value is skipped; the name-based fields are unaffected.
--
-- Fields are read here by their dotted names (ate.atespace, ate.actor.name).
-- Fluent Bit's opensearch output has Replace_Dots On, which turns dots into
-- underscores in every field name -- including nested keys, so
-- "openchoreo.dev/component-uid" written here lands as
-- "openchoreo_dev/component-uid" at query time, matching
-- observability-logs-opensearch's own ReplaceDots(ComponentID). That
-- replacement happens only at output time, so the dots are intact here.

local ENVIRONMENTS = { "development", "staging", "production" }
local ATTRIBUTION_DIR = "/etc/attribution"

local function actor_labels(record)
  return record["logging.googleapis.com/labels"] or record["labels"]
end

local function split_atespace(atespace)
  if not atespace then
    return nil, nil
  end
  local hash = atespace:match("%-(%x%x%x%x%x%x%x%x)$")
  if not hash then
    return nil, nil
  end
  local rest = atespace:sub(1, #atespace - #hash - 1)
  for _, env in ipairs(ENVIRONMENTS) do
    local suffix = "-" .. env
    if rest:sub(-#suffix) == suffix then
      local project = rest:sub(1, #rest - #suffix):match("%-([^%-]+)$")
      return project, env
    end
  end
  return nil, nil
end

local function component_from_actor_name(actorName, env)
  if not actorName or not env then
    return nil
  end
  return actorName:match("^(.-)%-" .. env .. "%-a%-%x%x%x%x%x%x%x%x%x%x$")
end

-- read_uid reads one attribution file's content, or nil if it doesn't exist
-- (ConfigMap volumes delete the file when the key is absent, so a missing
-- file is the normal "not recorded yet" case, not an error).
local function read_uid(filename)
  local f = io.open(ATTRIBUTION_DIR .. "/" .. filename, "r")
  if not f then
    return nil
  end
  local content = f:read("*a")
  f:close()
  return content and content:gsub("%s+$", "") or nil
end

function enrich(tag, timestamp, record)
  local labels = actor_labels(record)
  if not labels then
    return 0, 0, 0
  end

  local project, env = split_atespace(labels["ate.atespace"])
  if not env then
    return 0, 0, 0
  end

  record["openchoreo_environment"] = env
  if project then
    record["openchoreo_project"] = project
  end

  local component = component_from_actor_name(labels["ate.actor.name"], env)
  if component then
    record["openchoreo_component"] = component
  end

  local k8sLabels = record["kubernetes"]
  if k8sLabels then
    if not k8sLabels["labels"] then
      k8sLabels["labels"] = {}
    end
    k8sLabels = k8sLabels["labels"]

    local envUID = read_uid("environment." .. env)
    if envUID then
      k8sLabels["openchoreo.dev/environment-uid"] = envUID
    end
    if project then
      local projUID = read_uid("project." .. project)
      if projUID then
        k8sLabels["openchoreo.dev/project-uid"] = projUID
      end
      -- The component-logs query unconditionally requires this label,
      -- separate from project-uid even though the two happen to share a
      -- value in simple setups: it's the project's own Kubernetes
      -- namespace, not the project's name.
      local ns = read_uid("namespace." .. project)
      if ns then
        k8sLabels["openchoreo.dev/namespace"] = ns
      end
      if component then
        local compUID = read_uid("component." .. project .. "." .. env .. "." .. component)
        if compUID then
          k8sLabels["openchoreo.dev/component-uid"] = compUID
        end
      end
    end
  end

  return 1, timestamp, record
end
