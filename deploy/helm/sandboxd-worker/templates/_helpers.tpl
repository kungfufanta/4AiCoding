{{- define "sandboxd-worker.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "sandboxd-worker.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "sandboxd-worker.labels" -}}
{{ include "sandboxd-worker.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "sandboxd-worker.secretName" -}}
{{- if .Values.workerToken.existingSecret -}}
{{- .Values.workerToken.existingSecret -}}
{{- else -}}
{{- include "sandboxd-worker.fullname" . -}}
{{- end -}}
{{- end -}}

{{- define "sandboxd-worker.validate" -}}
{{- if not .Values.controlPlane.url -}}
{{- fail "controlPlane.url is required (the control plane API URL as seen from the pods)" -}}
{{- end -}}
{{- if and (not .Values.workerToken.value) (not .Values.workerToken.existingSecret) -}}
{{- fail "set workerToken.value or workerToken.existingSecret (the control plane's SANDBOXD_WORKER_TOKEN)" -}}
{{- end -}}
{{- if not (has .Values.kind (list "DaemonSet" "Deployment")) -}}
{{- fail "kind must be DaemonSet or Deployment" -}}
{{- end -}}
{{- if and (eq .Values.kind "Deployment") (not .Values.nodeId) -}}
{{- fail "kind=Deployment needs nodeId, so the node keeps its id across pod restarts" -}}
{{- end -}}
{{- if and .Values.executor.enabled (ne .Values.kind "DaemonSet") -}}
{{- fail "executor.enabled needs kind=DaemonSet (one executor per node, id = node name)" -}}
{{- end -}}
{{- if and .Values.executor.enabled (not .Values.executor.sandboxImage) -}}
{{- fail "executor.enabled needs executor.sandboxImage" -}}
{{- end -}}
{{- if and .Values.executor.enabled (not (has .Values.executor.engine (list "docker" "kubernetes"))) -}}
{{- fail "executor.engine must be docker or kubernetes" -}}
{{- end -}}
{{- if and (eq .Values.executor.engine "kubernetes") (not .Values.executor.enabled) -}}
{{- fail "executor.engine=kubernetes needs executor.enabled=true (only executors run sandboxes)" -}}
{{- end -}}
{{- if and .Values.nodeId (not (regexMatch "^[a-z0-9][a-z0-9.-]{0,62}$" .Values.nodeId)) -}}
{{- fail "nodeId must match [a-z0-9][a-z0-9.-]{0,62}" -}}
{{- end -}}
{{- end -}}
