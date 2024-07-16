package caddy_chrome

import (
	"fmt"
	"github.com/chromedp/cdproto/cdp"
	"html"
	"io"
	"strings"
)

// See https://developer.mozilla.org/en-US/docs/Glossary/Void_element
var voidElements = map[string]bool{
	"area":   true,
	"base":   true,
	"br":     true,
	"col":    true,
	"embed":  true,
	"hr":     true,
	"img":    true,
	"input":  true,
	"link":   true,
	"meta":   true,
	"param":  true,
	"source": true,
	"track":  true,
	"wbr":    true,
}

type domSerializer struct {
	root           *cdp.Node
	doctypeWritten bool
	noEscape       bool
}

func (s *domSerializer) Serialize(w io.Writer) error {
	return s.serializeNode(w, s.root)
}

func (s *domSerializer) serializeNode(w io.Writer, node *cdp.Node) error {
	switch node.NodeType {
	case cdp.NodeTypeElement:
		return s.serializeElementNode(w, node)
	case cdp.NodeTypeText:
		return s.serializeTextNode(w, node)
	case cdp.NodeTypeDocument:
		return s.serializeDocumentNode(w, node)
	case cdp.NodeTypeDocumentType:
		return s.serializeDocumentTypeNode(w, node)
	case cdp.NodeTypeDocumentFragment:
		return s.serializeChildren(w, node)
	default:
		return fmt.Errorf("node type [%d] not implemeted", node.NodeType)
	}
}

func (s *domSerializer) serializeDocumentNode(w io.Writer, node *cdp.Node) error {
	return s.serializeChildren(w, node)
}

func (s *domSerializer) serializeElementNode(w io.Writer, node *cdp.Node) error {
	if !s.doctypeWritten {
		if _, err := w.Write([]byte("<!DOCTYPE html>")); err != nil {
			return err
		}
		s.doctypeWritten = true
	}

	// start tag
	if _, err := w.Write([]byte(`<`)); err != nil {
		return err
	}
	localName := node.LocalName
	if _, err := w.Write([]byte(localName)); err != nil {
		return err
	}
	for i, l := 0, len(node.Attributes); i < l; i += 2 {
		if _, err := w.Write([]byte(` `)); err != nil {
			return err
		}
		attributeName := node.Attributes[i]
		if _, err := w.Write([]byte(attributeName)); err != nil {
			return err
		}
		if node.Attributes[i+1] != "" {
			if _, err := w.Write([]byte(`="`)); err != nil {
				return err
			}
			attributeValue := html.EscapeString(node.Attributes[i+1])
			if _, err := w.Write([]byte(attributeValue)); err != nil {
				return err
			}
			if _, err := w.Write([]byte(`"`)); err != nil {
				return err
			}
		}
	}
	isVoid := voidElements[strings.ToLower(localName)]
	if isVoid {
		if _, err := w.Write([]byte(` />`)); err != nil {
			return err
		}
	} else {
		if _, err := w.Write([]byte(`>`)); err != nil {
			return err
		}
	}

	// shadow roots
	for _, shadowRoot := range node.ShadowRoots {
		if shadowRoot.ShadowRootType != "open" && shadowRoot.ShadowRootType != "closed" {
			continue
		}

		if _, err := w.Write([]byte(`<template shadowrootmode="`)); err != nil {
			return err
		}
		if _, err := w.Write([]byte(shadowRoot.ShadowRootType)); err != nil {
			return err
		}
		if _, err := w.Write([]byte(`">`)); err != nil {
			return err
		}
		if err := s.serializeNode(w, shadowRoot); err != nil {
			return err
		}
		if _, err := w.Write([]byte(`</template>`)); err != nil {
			return err
		}
	}

	// children
	if localName == "script" || localName == "style" {
		savedNoEscape := s.noEscape
		s.noEscape = true
		defer func() {
			s.noEscape = savedNoEscape
		}()
	}
	if err := s.serializeChildren(w, node); err != nil {
		return err
	}

	// end tag
	if !isVoid {
		if _, err := w.Write([]byte("</")); err != nil {
			return err
		}
		if _, err := w.Write([]byte(localName)); err != nil {
			return err
		}
		if _, err := w.Write([]byte(">")); err != nil {
			return err
		}
	}

	return nil
}

func (s *domSerializer) serializeChildren(w io.Writer, node *cdp.Node) error {
	for _, child := range node.Children {
		if err := s.serializeNode(w, child); err != nil {
			return err
		}
	}
	return nil
}

func (s *domSerializer) serializeDocumentTypeNode(w io.Writer, node *cdp.Node) error {
	if _, err := w.Write([]byte("<!DOCTYPE ")); err != nil {
		return err
	}
	if _, err := w.Write([]byte(node.NodeName)); err != nil {
		return err
	}
	if _, err := w.Write([]byte(">")); err != nil {
		return err
	}
	s.doctypeWritten = true
	return nil
}

func (s *domSerializer) serializeTextNode(w io.Writer, node *cdp.Node) error {
	var text string
	if s.noEscape {
		text = node.NodeValue
	} else {
		text = html.EscapeString(node.NodeValue)
	}
	if _, err := w.Write([]byte(text)); err != nil {
		return err
	}
	return nil
}
