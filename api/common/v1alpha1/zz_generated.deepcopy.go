//go:build !ignore_autogenerated

// Copyright (C) 2025 The Falco Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

// Package controller defines controllers' logic.

// Code generated by controller-gen. DO NOT EDIT.

package v1alpha1

import ()

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *OCIArtifact) DeepCopyInto(out *OCIArtifact) {
	*out = *in
	if in.PullSecret != nil {
		in, out := &in.PullSecret, &out.PullSecret
		*out = new(OCIPullSecret)
		**out = **in
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new OCIArtifact.
func (in *OCIArtifact) DeepCopy() *OCIArtifact {
	if in == nil {
		return nil
	}
	out := new(OCIArtifact)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *OCIPullSecret) DeepCopyInto(out *OCIPullSecret) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new OCIPullSecret.
func (in *OCIPullSecret) DeepCopy() *OCIPullSecret {
	if in == nil {
		return nil
	}
	out := new(OCIPullSecret)
	in.DeepCopyInto(out)
	return out
}
