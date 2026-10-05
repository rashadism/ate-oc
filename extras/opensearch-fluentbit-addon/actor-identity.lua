-- Derives openchoreo_component / openchoreo_project / openchoreo_environment
-- (names) plus the real openchoreo.dev/*-uid Kubernetes labels for actor log
-- lines, the same way the kubernetes filter derives both from pod labels for
-- a normal component.
--
-- internal/actorlog nests the identity fields under one of two label-group
-- keys depending on environment (internal/actorlog/logger.go LabelsKey):
-- "logging.googleapis.com/labels" on GCE, "labels" elsewhere.
--
-- ate.atespace and ate.actor.name are opaque strings here, looked up
-- verbatim -- never split. Component, project, and environment names are
-- free-form Kubernetes names that may themselves contain hyphens (nothing
-- stops a component named "my-app-prod"), so there is no delimiter that
-- could tell "my-app-prod"'s own hyphens apart from a separator between
-- fields. oc-substrate's ActorTemplate controller already knows each
-- atespace's and actor name's real identity at reconcile time with no
-- ambiguity, and publishes both, keyed by the exact string, into a
-- ConfigMap mounted at ATTRIBUTION_DIR (one file per entry, the key as the
-- filename). A missing file just means that lookup is skipped.
--
-- Fields are read here by their dotted names (ate.atespace, ate.actor.name).
-- Fluent Bit's opensearch output has Replace_Dots On, which turns dots into
-- underscores in every field name -- including nested keys, so
-- "openchoreo.dev/component-uid" written here lands as
-- "openchoreo_dev/component-uid" at query time, matching
-- observability-logs-opensearch's own ReplaceDots(ComponentID). That
-- replacement happens only at output time, so the dots are intact here.

local ATTRIBUTION_DIR = "/etc/attribution"

local function actor_labels(record)
  return record["logging.googleapis.com/labels"] or record["labels"]
end

-- read_entry reads one attribution file's content, or nil if it doesn't
-- exist (ConfigMap volumes delete the file when the key is absent, so a
-- missing file is the normal "not recorded yet" case, not an error).
local function read_entry(filename)
  local f = io.open(ATTRIBUTION_DIR .. "/" .. filename, "r")
  if not f then
    return nil
  end
  local content = f:read("*a")
  f:close()
  return content and content:gsub("%s+$", "") or nil
end

-- split_pair splits a "a|b" attribution value. Entries that pair two values
-- (name and uid) use this; "|" is not a valid character in a Kubernetes
-- name, so it is an unambiguous separator here even though a hyphen is not.
local function split_pair(value)
  if not value then
    return nil, nil
  end
  local a, b = value:match("^([^|]*)|([^|]*)$")
  return a, b
end

function enrich(tag, timestamp, record)
  local labels = actor_labels(record)
  if not labels then
    return 0, 0, 0
  end

  local atespace = labels["ate.atespace"]
  local actorName = labels["ate.actor.name"]

  local project, env = split_pair(read_entry("atespace." .. (atespace or "")))
  if env then
    record["openchoreo_environment"] = env
  end
  if project then
    record["openchoreo_project"] = project
  end

  local component, componentUID = split_pair(read_entry("actorname." .. (actorName or "")))
  if component then
    record["openchoreo_component"] = component
  end

  local k8sLabels = record["kubernetes"]
  if k8sLabels then
    if not k8sLabels["labels"] then
      k8sLabels["labels"] = {}
    end
    k8sLabels = k8sLabels["labels"]

    if env then
      local envUID = read_entry("environment." .. env)
      if envUID then
        k8sLabels["openchoreo.dev/environment-uid"] = envUID
      end
    end
    if project then
      local projUID = read_entry("project." .. project)
      if projUID then
        k8sLabels["openchoreo.dev/project-uid"] = projUID
      end
      -- The component-logs query unconditionally requires this label,
      -- separate from project-uid even though the two happen to share a
      -- value in simple setups: it's the project's own Kubernetes
      -- namespace, not the project's name.
      local namespace = read_entry("namespace." .. project)
      if namespace then
        k8sLabels["openchoreo.dev/namespace"] = namespace
      end
    end
    if componentUID then
      k8sLabels["openchoreo.dev/component-uid"] = componentUID
    end
  end

  if not (env or component) then
    return 0, 0, 0
  end
  return 1, timestamp, record
end
