{{- define "cosi-driver-qumulo.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cosi-driver-qumulo.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "cosi-driver-qumulo.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "cosi-driver-qumulo.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "cosi-driver-qumulo.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cosi-driver-qumulo.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "cosi-driver-qumulo.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "cosi-driver-qumulo.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "cosi-driver-qumulo.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- end -}}
{{- end -}}

{{- define "cosi-driver-qumulo.csiName" -}}
{{- $prefix := include "cosi-driver-qumulo.fullname" . | trunc 59 | trimSuffix "-" -}}
{{- printf "%s-csi" $prefix -}}
{{- end -}}

{{- define "cosi-driver-qumulo.csiControllerName" -}}
{{- $prefix := include "cosi-driver-qumulo.fullname" . | trunc 48 | trimSuffix "-" -}}
{{- printf "%s-csi-controller" $prefix -}}
{{- end -}}

{{- define "cosi-driver-qumulo.csiNodeName" -}}
{{- $prefix := include "cosi-driver-qumulo.fullname" . | trunc 54 | trimSuffix "-" -}}
{{- printf "%s-csi-node" $prefix -}}
{{- end -}}

{{- define "cosi-driver-qumulo.csiProvisionerName" -}}
{{- $prefix := include "cosi-driver-qumulo.fullname" . | trunc 47 | trimSuffix "-" -}}
{{- printf "%s-csi-provisioner" $prefix -}}
{{- end -}}

{{- define "cosi-driver-qumulo.csiLeaderElectionName" -}}
{{- $prefix := include "cosi-driver-qumulo.fullname" . | trunc 43 | trimSuffix "-" -}}
{{- printf "%s-csi-leader-election" $prefix -}}
{{- end -}}

{{- define "cosi-driver-qumulo.csiSelectorLabels" -}}
app.kubernetes.io/name: {{ include "cosi-driver-qumulo.csiName" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "cosi-driver-qumulo.csiControllerServiceAccountName" -}}
{{- if .Values.csi.serviceAccount.create -}}
{{- default (include "cosi-driver-qumulo.csiControllerName" .) .Values.csi.serviceAccount.controllerName -}}
{{- else -}}
{{- required "csi.serviceAccount.controllerName is required when csi.serviceAccount.create=false" .Values.csi.serviceAccount.controllerName -}}
{{- end -}}
{{- end -}}

{{- define "cosi-driver-qumulo.csiNodeServiceAccountName" -}}
{{- if .Values.csi.serviceAccount.create -}}
{{- default (include "cosi-driver-qumulo.csiNodeName" .) .Values.csi.serviceAccount.nodeName -}}
{{- else -}}
{{- required "csi.serviceAccount.nodeName is required when csi.serviceAccount.create=false" .Values.csi.serviceAccount.nodeName -}}
{{- end -}}
{{- end -}}

{{- define "cosi-driver-qumulo.csiCredentialsSecret" -}}
{{- default .Values.qumulo.existingSecret .Values.csi.existingSecret -}}
{{- end -}}

{{/*
CSI runtime image: alpine + mount helpers, published beside the COSI image
with a "-csi" tag suffix. Override with csi.image / csi.imageDigest.
*/}}
{{- define "cosi-driver-qumulo.csiImage" -}}
{{- if .Values.csi.imageDigest -}}
{{- printf "%s@%s" .Values.image.repository .Values.csi.imageDigest -}}
{{- else if .Values.csi.image -}}
{{- .Values.csi.image -}}
{{- else -}}
{{- printf "%s:%s-csi" .Values.image.repository .Values.image.tag -}}
{{- end -}}
{{- end -}}
