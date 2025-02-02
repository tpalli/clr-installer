// Copyright © 2018 Intel Corporation
//
// SPDX-License-Identifier: GPL-3.0-only

package tui

import (
	"github.com/VladimirMarkelov/clui"

	"github.com/clearlinux/clr-installer/swupd"
)

// SwupdMirrorPage is the Page implementation for the swupd mirror page
type SwupdMirrorPage struct {
	BasePage
	swupdMirrorEdit    *clui.EditField
	swupdMirrorWarning *clui.Label
	confirmBtn         *SimpleButton
	cancelBtn          *SimpleButton
	userDefined        bool
}

// GetConfiguredValue Returns the string representation of currently value set
func (page *SwupdMirrorPage) GetConfiguredValue() string {
	mirror := page.getModel().SwupdMirror

	if mirror == "" {
		return "No swupd mirror set"
	}

	return mirror
}

// GetConfigDefinition returns if the config was interactively defined by the user,
// was loaded from a config file or if the config is not set.
func (page *SwupdMirrorPage) GetConfigDefinition() int {
	mirror := page.getModel().SwupdMirror

	if mirror == "" {
		return ConfigNotDefined
	} else if page.userDefined {
		return ConfigDefinedByUser
	}

	return ConfigDefinedByConfig
}

// Activate sets the swupd mirror with the current model's value
func (page *SwupdMirrorPage) Activate() {
	page.swupdMirrorEdit.SetTitle(page.getModel().SwupdMirror)
	page.swupdMirrorWarning.SetTitle("")
}

func (page *SwupdMirrorPage) setConfirmButton() {
	if page.swupdMirrorWarning.Title() == "" {
		page.confirmBtn.SetEnabled(true)
	} else {
		page.confirmBtn.SetEnabled(false)
	}
}

func newSwupdMirrorPage(tui *Tui) (Page, error) {
	page := &SwupdMirrorPage{}
	page.setupMenu(tui, TuiPageSwupdMirror, "Swupd Mirror", NoButtons, TuiPageMenu)
	clui.CreateLabel(page.content, 2, 2, swupd.MirrorDesc1, Fixed)

	frm := clui.CreateFrame(page.content, AutoSize, AutoSize, BorderNone, Fixed)
	frm.SetPack(clui.Horizontal)

	lblFrm := clui.CreateFrame(frm, 20, AutoSize, BorderNone, Fixed)
	lblFrm.SetPack(clui.Vertical)
	lblFrm.SetPaddings(1, 0)
	title := swupd.MirrorTitle + ":"
	newFieldLabel(lblFrm, title)

	fldFrm := clui.CreateFrame(frm, 30, AutoSize, BorderNone, Fixed)
	fldFrm.SetPack(clui.Vertical)

	iframe := clui.CreateFrame(fldFrm, 5, 2, BorderNone, Fixed)
	iframe.SetPack(clui.Vertical)

	page.swupdMirrorEdit = clui.CreateEditField(iframe, 1, "", Fixed)
	page.swupdMirrorEdit.OnChange(func(ev clui.Event) {
		warning := ""
		userURL := page.swupdMirrorEdit.Title()

		if userURL != "" && swupd.IsValidMirror(userURL) == false {
			warning = swupd.InvalidURL
		}

		page.swupdMirrorWarning.SetTitle(warning)
		page.setConfirmButton()
	})

	page.swupdMirrorWarning = clui.CreateLabel(iframe, 1, 1, "", Fixed)
	page.swupdMirrorWarning.SetMultiline(true)
	page.swupdMirrorWarning.SetBackColor(errorLabelBg)
	page.swupdMirrorWarning.SetTextColor(errorLabelFg)
	lbl := clui.CreateLabel(iframe, 2, 11, swupd.MirrorDesc2, Fixed)
	lbl.SetMultiline(true)

	btnFrm := clui.CreateFrame(fldFrm, 50, 1, BorderNone, Fixed)
	btnFrm.SetPack(clui.Horizontal)
	btnFrm.SetGaps(1, 1)
	btnFrm.SetPaddings(2, 0)

	page.cancelBtn = CreateSimpleButton(btnFrm, AutoSize, AutoSize, "Cancel", Fixed)
	page.cancelBtn.OnClick(func(ev clui.Event) {
		page.GotoPage(TuiPageMenu)
	})

	page.confirmBtn = CreateSimpleButton(btnFrm, AutoSize, AutoSize, "Confirm", Fixed)
	page.confirmBtn.OnClick(func(ev clui.Event) {
		page.confirmBtn.SetEnabled(false)
		mirror := page.swupdMirrorEdit.Title()
		if mirror == "" {
			if page.getModel().SwupdMirror != "" {
				_, _ = swupd.UnSetHostMirror()
				page.getModel().SwupdMirror = mirror
			}
			page.SetDone(false)
			page.GotoPage(TuiPageMenu)
			page.userDefined = false
		} else {
			url, err := swupd.SetHostMirror(mirror)
			if err != nil {
				page.swupdMirrorWarning.SetTitle(err.Error())
			} else {
				if url != mirror {
					page.swupdMirrorWarning.SetTitle(swupd.IncorrectMirror + ": " + url)
				} else {
					page.userDefined = true
					page.getModel().SwupdMirror = mirror
					page.SetDone(true)
					page.GotoPage(TuiPageMenu)
				}
			}
		}
		page.setConfirmButton()
	})
	page.confirmBtn.SetEnabled(false)

	page.activated = page.swupdMirrorEdit

	return page, nil
}
