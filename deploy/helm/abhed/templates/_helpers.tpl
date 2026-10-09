{{- define "abhed.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "abhed.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "abhed.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "abhed.selectorLabels" -}}
app.kubernetes.io/name: {{ include "abhed.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "abhed.labels" -}}
helm.sh/chart: {{ include "abhed.chart" . }}
{{ include "abhed.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: abhed
{{- end }}

{{- define "abhed.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "abhed.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The image by digest when one is given, else by tag (v<appVersion> by default). */}}
{{- define "abhed.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default (printf "v%s" .Chart.AppVersion) .Values.image.tag) }}
{{- end }}
{{- end }}

{{/* An HTTP probe of path, or a TCP one when the server requires a client
certificate, which the kubelet cannot present. */}}
{{- define "abhed.probe" -}}
{{- $ := index . 0 }}
{{- $tls := $.Values.tls }}
{{- if and $tls.enabled $tls.clientCA.existingSecret (eq $tls.clientCA.clientAuth "require") }}
tcpSocket:
  port: http
{{- else }}
httpGet:
  path: {{ index . 1 }}
  port: http
  scheme: {{ ternary "HTTPS" "HTTP" $tls.enabled }}
{{- end }}
{{- end }}

{{/* The configuration as mounted: the operator's config plus what the chart's own values decide. */}}
{{- define "abhed.config" -}}
{{- $cfg := deepCopy (default (dict) .Values.config) }}
{{- if .Values.tls.enabled }}
{{- $server := default (dict) (get $cfg "server") }}
{{- $t := default (dict) (get $server "tls") }}
{{- $_ := set $t "cert_file" (printf "/etc/abhed/tls/%s" .Values.tls.certKey) }}
{{- $_ := set $t "key_file" (printf "/etc/abhed/tls/%s" .Values.tls.keyKey) }}
{{- if .Values.tls.minVersion }}
{{- $_ := set $t "min_version" .Values.tls.minVersion }}
{{- end }}
{{- if .Values.tls.clientCA.existingSecret }}
{{- $_ := set $t "client_ca_file" (printf "/etc/abhed/client-ca/%s" .Values.tls.clientCA.key) }}
{{- $_ := set $t "client_auth" .Values.tls.clientCA.clientAuth }}
{{- end }}
{{- $_ := set $server "tls" $t }}
{{- $_ := set $cfg "server" $server }}
{{- end }}
{{- if .Values.postgres.enabled }}
{{- $storage := default (dict) (get $cfg "storage") }}
{{- $_ := set $storage "driver" "postgres" }}
{{- $_ := set $cfg "storage" $storage }}
{{- end }}
{{- if .Values.controlPlane.sshHostKey.existingSecret }}
{{- $cp := default (dict) (get $cfg "control_plane") }}
{{- $_ := set $cp "ssh_host_key" (printf "/run/abhed/secrets/%s" (include "abhed.sshHostKeyPath" .)) }}
{{- $_ := set $cfg "control_plane" $cp }}
{{- end }}
{{- toPrettyJson $cfg }}
{{- end }}

{{/* Where the relay's host key is copied, under /run/abhed/secrets. */}}
{{- define "abhed.sshHostKeyPath" -}}
ssh_host_ed25519_key
{{- end }}

{{/* secretFiles with the relay's host key added, as a JSON array. */}}
{{- define "abhed.secretFiles" -}}
{{- $files := list }}
{{- range .Values.secretFiles }}
{{- $files = append $files . }}
{{- end }}
{{- with .Values.controlPlane.sshHostKey.existingSecret }}
{{- $files = append $files (dict "secretName" . "key" $.Values.controlPlane.sshHostKey.key "path" (include "abhed.sshHostKeyPath" $)) }}
{{- end }}
{{- toJson $files }}
{{- end }}

{{/* Refuse values that would put a secret in the ConfigMap or run the chart in a shape that cannot work. */}}
{{- define "abhed.validate" -}}
{{- $json := toJson (default (dict) .Values.config) }}
{{- $podJSON := toJson (default (dict) .Values.podConfig) }}
{{- $secretKey := `"(api_key|apikey|dsn|migrate_dsn|password|client_secret|secret|token|private_key)"\s*:` }}
{{- if regexMatch $secretKey $json }}
{{- fail "config holds a secret (api_key, dsn, password, client_secret, token, ...). Keep secrets out of the ConfigMap: name a variable (api_key_env, client_secret_env) and fill it with secretEnv, or use postgres.existingSecret for the database." }}
{{- end }}
{{- if regexMatch $secretKey $podJSON }}
{{- fail "podConfig holds a secret; use secretEnv or secretFiles." }}
{{- end }}
{{- $storage := default (dict) (get (default (dict) .Values.config) "storage") }}
{{- if and (eq (toString (get $storage "driver")) "postgres") (not .Values.postgres.enabled) }}
{{- fail "config.storage.driver is postgres: set postgres.enabled and postgres.existingSecret so the connection string comes from a Secret." }}
{{- end }}
{{- if and .Values.postgres.enabled (not .Values.postgres.existingSecret) }}
{{- fail "postgres.enabled needs postgres.existingSecret: the chart never creates or templates a secret." }}
{{- end }}
{{- if and .Values.tls.enabled (not .Values.tls.existingSecret) }}
{{- fail "tls.enabled needs tls.existingSecret, a kubernetes.io/tls Secret the chart mounts at /etc/abhed/tls." }}
{{- end }}
{{- if and (hasKey (default (dict) (get (default (dict) .Values.config) "server")) "tls") (not .Values.tls.enabled) }}
{{- fail "config.server.tls is set: use the tls values instead, so the key is mounted from a Secret and the probes speak HTTPS." }}
{{- end }}
{{- if gt (int .Values.replicaCount) 1 }}
{{- if not .Values.postgres.enabled }}
{{- fail "replicaCount above 1 needs postgres.enabled: replicas share sessions, the record and sandboxes through Postgres." }}
{{- end }}
{{- if and .Values.persistence.enabled (not (has "ReadWriteMany" .Values.persistence.accessModes)) }}
{{- fail "replicaCount above 1 cannot share a ReadWriteOnce volume: set persistence.accessModes to [ReadWriteMany] or persistence.enabled false." }}
{{- end }}
{{- end }}
{{- if and .Values.podDisruptionBudget.enabled (toString .Values.podDisruptionBudget.minAvailable) (toString .Values.podDisruptionBudget.maxUnavailable) }}
{{- fail "podDisruptionBudget: set minAvailable or maxUnavailable, not both." }}
{{- end }}
{{- if and .Values.route.enabled .Values.tls.enabled (eq .Values.route.tls.termination "edge") }}
{{- fail "route.tls.termination edge sends plain HTTP to a pod that serves TLS: use passthrough or reencrypt with tls.enabled." }}
{{- end }}
{{- $cpCfg := default (dict) (get (default (dict) .Values.config) "control_plane") }}
{{- if hasKey $cpCfg "ssh_host_key" }}
{{- fail "config.control_plane.ssh_host_key is set: use controlPlane.sshHostKey.existingSecret, so the key is copied in from a Secret, mode 0400." }}
{{- end }}
{{- if and .Values.controlPlane.sshHostKey.existingSecret (not (get $cpCfg "enabled")) }}
{{- fail "controlPlane.sshHostKey needs config.control_plane.enabled: the key is for the control plane's :ssh relay." }}
{{- end }}
{{- $paths := dict }}
{{- range (include "abhed.secretFiles" . | fromJsonArray) }}
{{- if hasKey $paths .path }}
{{- fail (printf "secretFiles: path %s is used twice (controlPlane.sshHostKey takes %s)" .path (include "abhed.sshHostKeyPath" $)) }}
{{- end }}
{{- $_ := set $paths .path true }}
{{- end }}
{{- end }}

{{/* Pod environment shared by the server and its init containers. */}}
{{- define "abhed.env" -}}
- name: HOME
  value: /home/abhed
- name: ABHED_IN_CONTAINER
  value: "1"
{{- if .Values.postgres.enabled }}
- name: ABHED_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.postgres.existingSecret }}
      key: {{ .Values.postgres.key }}
{{- end }}
{{- range .Values.secretEnv }}
- name: {{ .name }}
  valueFrom:
    secretKeyRef:
      name: {{ .secretName }}
      key: {{ .key }}
{{- end }}
{{- with .Values.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/* Volume mounts shared by the server and the preflight check. */}}
{{- define "abhed.mounts" -}}
- name: config
  mountPath: /etc/abhed/config.json
  subPath: config.json
  readOnly: true
- name: data
  mountPath: /workspace
  subPath: workspace
- name: home
  mountPath: /home/abhed
- name: data
  mountPath: /home/abhed/.abhed
  subPath: state
- name: tmp
  mountPath: /tmp
{{- if .Values.tls.enabled }}
- name: tls
  mountPath: /etc/abhed/tls
  readOnly: true
{{- if .Values.tls.clientCA.existingSecret }}
- name: client-ca
  mountPath: /etc/abhed/client-ca
  readOnly: true
{{- end }}
{{- end }}
{{- if include "abhed.secretFiles" . | fromJsonArray }}
- name: secret-files
  mountPath: /run/abhed/secrets
  readOnly: true
{{- end }}
{{- with .Values.extraVolumeMounts }}
{{ toYaml . }}
{{- end }}
{{- end }}
