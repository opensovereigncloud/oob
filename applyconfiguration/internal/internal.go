/*
SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
SPDX-License-Identifier: Apache-2.0
*/

// Code generated by applyconfiguration-gen. DO NOT EDIT.

package internal

import (
	"fmt"
	"sync"

	typed "sigs.k8s.io/structured-merge-diff/v4/typed"
)

func Parser() *typed.Parser {
	parserOnce.Do(func() {
		var err error
		parser, err = typed.NewParser(schemaYAML)
		if err != nil {
			panic(fmt.Sprintf("Failed to parse schema: %v", err))
		}
	})
	return parser
}

var parserOnce sync.Once
var parser *typed.Parser
var schemaYAML = typed.YAMLObject(`types:
- name: com.github.ironcore-dev.oob.api.v1alpha1.OOB
  map:
    fields:
    - name: apiVersion
      type:
        scalar: string
    - name: kind
      type:
        scalar: string
    - name: metadata
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta
      default: {}
    - name: spec
      type:
        namedType: com.github.ironcore-dev.oob.api.v1alpha1.OOBSpec
      default: {}
    - name: status
      type:
        namedType: com.github.ironcore-dev.oob.api.v1alpha1.OOBStatus
      default: {}
- name: com.github.ironcore-dev.oob.api.v1alpha1.OOBSpec
  map:
    fields:
    - name: filler
      type:
        scalar: numeric
    - name: locatorLED
      type:
        scalar: string
    - name: power
      type:
        scalar: string
    - name: reset
      type:
        scalar: string
- name: com.github.ironcore-dev.oob.api.v1alpha1.OOBStatus
  map:
    fields:
    - name: capabilities
      type:
        list:
          elementType:
            scalar: string
          elementRelationship: atomic
    - name: conditions
      type:
        list:
          elementType:
            namedType: io.k8s.apimachinery.pkg.apis.meta.v1.Condition
          elementRelationship: associative
          keys:
          - type
    - name: console
      type:
        scalar: string
    - name: fwVersion
      type:
        scalar: string
    - name: ip
      type:
        scalar: string
    - name: locatorLED
      type:
        scalar: string
    - name: mac
      type:
        scalar: string
    - name: manufacturer
      type:
        scalar: string
    - name: os
      type:
        scalar: string
    - name: osReadDeadline
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.Time
    - name: osReason
      type:
        scalar: string
    - name: port
      type:
        scalar: numeric
    - name: power
      type:
        scalar: string
    - name: protocol
      type:
        scalar: string
    - name: serialNumber
      type:
        scalar: string
    - name: shutdownDeadline
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.Time
    - name: sku
      type:
        scalar: string
    - name: tags
      type:
        list:
          elementType:
            namedType: com.github.ironcore-dev.oob.api.v1alpha1.TagSpec
          elementRelationship: atomic
    - name: type
      type:
        scalar: string
    - name: uuid
      type:
        scalar: string
- name: com.github.ironcore-dev.oob.api.v1alpha1.TagSpec
  map:
    fields:
    - name: key
      type:
        scalar: string
    - name: value
      type:
        scalar: string
- name: io.k8s.apimachinery.pkg.apis.meta.v1.Condition
  map:
    fields:
    - name: lastTransitionTime
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.Time
    - name: message
      type:
        scalar: string
      default: ""
    - name: observedGeneration
      type:
        scalar: numeric
    - name: reason
      type:
        scalar: string
      default: ""
    - name: status
      type:
        scalar: string
      default: ""
    - name: type
      type:
        scalar: string
      default: ""
- name: io.k8s.apimachinery.pkg.apis.meta.v1.FieldsV1
  map:
    elementType:
      scalar: untyped
      list:
        elementType:
          namedType: __untyped_atomic_
        elementRelationship: atomic
      map:
        elementType:
          namedType: __untyped_deduced_
        elementRelationship: separable
- name: io.k8s.apimachinery.pkg.apis.meta.v1.ManagedFieldsEntry
  map:
    fields:
    - name: apiVersion
      type:
        scalar: string
    - name: fieldsType
      type:
        scalar: string
    - name: fieldsV1
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.FieldsV1
    - name: manager
      type:
        scalar: string
    - name: operation
      type:
        scalar: string
    - name: subresource
      type:
        scalar: string
    - name: time
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.Time
- name: io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta
  map:
    fields:
    - name: annotations
      type:
        map:
          elementType:
            scalar: string
    - name: creationTimestamp
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.Time
    - name: deletionGracePeriodSeconds
      type:
        scalar: numeric
    - name: deletionTimestamp
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.Time
    - name: finalizers
      type:
        list:
          elementType:
            scalar: string
          elementRelationship: associative
    - name: generateName
      type:
        scalar: string
    - name: generation
      type:
        scalar: numeric
    - name: labels
      type:
        map:
          elementType:
            scalar: string
    - name: managedFields
      type:
        list:
          elementType:
            namedType: io.k8s.apimachinery.pkg.apis.meta.v1.ManagedFieldsEntry
          elementRelationship: atomic
    - name: name
      type:
        scalar: string
    - name: namespace
      type:
        scalar: string
    - name: ownerReferences
      type:
        list:
          elementType:
            namedType: io.k8s.apimachinery.pkg.apis.meta.v1.OwnerReference
          elementRelationship: associative
          keys:
          - uid
    - name: resourceVersion
      type:
        scalar: string
    - name: selfLink
      type:
        scalar: string
    - name: uid
      type:
        scalar: string
- name: io.k8s.apimachinery.pkg.apis.meta.v1.OwnerReference
  map:
    fields:
    - name: apiVersion
      type:
        scalar: string
      default: ""
    - name: blockOwnerDeletion
      type:
        scalar: boolean
    - name: controller
      type:
        scalar: boolean
    - name: kind
      type:
        scalar: string
      default: ""
    - name: name
      type:
        scalar: string
      default: ""
    - name: uid
      type:
        scalar: string
      default: ""
    elementRelationship: atomic
- name: io.k8s.apimachinery.pkg.apis.meta.v1.Time
  scalar: untyped
- name: __untyped_atomic_
  scalar: untyped
  list:
    elementType:
      namedType: __untyped_atomic_
    elementRelationship: atomic
  map:
    elementType:
      namedType: __untyped_atomic_
    elementRelationship: atomic
- name: __untyped_deduced_
  scalar: untyped
  list:
    elementType:
      namedType: __untyped_atomic_
    elementRelationship: atomic
  map:
    elementType:
      namedType: __untyped_deduced_
    elementRelationship: separable
`)
