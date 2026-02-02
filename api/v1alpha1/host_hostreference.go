package v1alpha1

func (h *Host) SetHostReferenceNamespace(name string) {
	h.EnsureHostReference()
	h.Status.HostReference.Namespace = name
}

func (h *Host) SetHostReferenceHostPool(poolName string) {
	h.EnsureHostReference()
	h.Status.HostReference.HostPool = poolName
}

func (h *Host) EnsureHostReference() {
	if h.Status.HostReference == nil {
		h.Status.HostReference = &HostReferenceType{}
	}
}

func (h *Host) GetHostReferenceNamespace() string {
	if h.Status.HostReference == nil {
		return ""
	}
	return h.Status.HostReference.Namespace
}

func (h *Host) GetHostReferenceHostPool() string {
	if h.Status.HostReference == nil {
		return ""
	}
	return h.Status.HostReference.HostPool
}
