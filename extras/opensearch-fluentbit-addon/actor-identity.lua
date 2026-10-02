-- Derives openchoreo_component / openchoreo_project / openchoreo_environment
-- for actor log lines from Substrate's own identity fields, the same way
-- the kubernetes filter derives them from pod labels for a normal
-- component.
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
-- Fields are read here by their dotted names (ate.atespace, ate.actor.name).
-- Fluent Bit's opensearch output has Replace_Dots On, which turns dots into
-- underscores in every field name -- but only at output time. This filter
-- runs earlier, against the record as the json parser decoded it, so the
-- dots are intact.

local ENVIRONMENTS = { "development", "staging", "production" }

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

  return 1, timestamp, record
end
