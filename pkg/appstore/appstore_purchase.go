package appstore

import (
	"errors"
	"fmt"
	gohttp "net/http"
	"strings"

	"github.com/majd/ipatool/v2/pkg/http"
)

var (
	ErrPasswordTokenExpired   = errors.New("password token is expired")
	ErrLicenseAlreadyExists   = errors.New("license already exists")
	ErrSubscriptionRequired   = errors.New("subscription required")
	ErrTemporarilyUnavailable = errors.New("item is temporarily unavailable")
	ErrPurchaseValidation     = errors.New("purchase requires secondary validation (e.g., CVV or 2FA)")
)

// 兼容版 PurchaseInput
type PurchaseInput struct {
	Account Account
	App     App // 宿主 App (購買普通 App 時，依賴這裡的 ID 和 Price)

	// 以下為擴充參數 (購買內購/訂閱時使用，購買普通 App 時可留空)
	SalableAdamId     int64  // 內購項目 ID (若為 0，則自動退回使用 App.ID)
	Price             string // 實際價格字串 (若為空，則自動退回使用 App.Price)
	ProductType       string // 產品類型："C", "S", "I" (若為空，則自動退回 "C")
	AppExtVrsId       string // 當前 App 版本 ID (若為空，則自動退回 "0")
	PricingParameters string // 若為空，則自動退回 PricingParameterAppStore
	ActionSignature   string // 付費購買必填的 X-Apple-ActionSignature
}

func (t *appstore) Purchase(input PurchaseInput) error {
	macAddr, err := t.machine.MacAddress()
	if err != nil {
		return fmt.Errorf("failed to get mac address: %w", err)
	}

	guid := strings.ReplaceAll(strings.ToUpper(macAddr), ":", "")

	// 決定使用的 PricingParameters (兼容舊版邏輯)
	buyParams := input.PricingParameters
	if buyParams == "" {
		buyParams = PricingParameterAppStore
	}

	// 呼叫底層
	err = t.purchaseWithParams(input, guid, buyParams)
	if err != nil {
		if err == ErrTemporarilyUnavailable {
			err = t.purchaseWithParams(input, guid, PricingParameterAppleArcade)
			if err != nil {
				return fmt.Errorf("failed to purchase item with param '%s': %w", PricingParameterAppleArcade, err)
			}
			return nil
		}
		return fmt.Errorf("failed to purchase item with param '%s': %w", PricingParameterAppStore, err)
	}

	return nil
}

type purchaseResult struct {
	FailureType     string `plist:"failureType,omitempty"`
	CustomerMessage string `plist:"customerMessage,omitempty"`
	JingleDocType   string `plist:"jingleDocType,omitempty"`
	Status          int    `plist:"status,omitempty"`
}

func (t *appstore) purchaseWithParams(input PurchaseInput, guid string, pricingParameters string) error {
	req := t.purchaseRequest(input, input.Account.StoreFront, guid, pricingParameters)
	res, err := t.purchaseClient.Send(req)

	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}

	// --- 狀態處理邏輯保持不變 ---
	if res.Data.FailureType == FailureTypeTemporarilyUnavailable {
		return ErrTemporarilyUnavailable
	}
	if res.Data.CustomerMessage == CustomerMessageSubscriptionRequired {
		return ErrSubscriptionRequired
	}
	if res.Data.FailureType == FailureTypePasswordTokenExpired ||
		res.Data.FailureType == FailureTypeSignInRequired ||
		res.Data.FailureType == FailureTypeDeviceVerificationFailed ||
		res.Data.CustomerMessage == CustomerMessagePasswordChanged {
		return ErrPasswordTokenExpired
	}
	if res.Data.FailureType == FailureTypeLicenseAlreadyExists {
		return ErrLicenseAlreadyExists
	}
	// 攔截二次驗證狀態
	if res.Data.Status == 1 || strings.Contains(res.Data.FailureType, "Validation") {
		return ErrPurchaseValidation
	}
	if res.Data.FailureType != "" && res.Data.CustomerMessage != "" {
		return NewErrorWithMetadata(errors.New(res.Data.CustomerMessage), res)
	}
	if res.Data.FailureType != "" {
		return NewErrorWithMetadata(errors.New("something went wrong"), res)
	}
	if res.StatusCode == gohttp.StatusInternalServerError {
		return ErrLicenseAlreadyExists
	}
	if res.Data.JingleDocType != "purchaseSuccess" || res.Data.Status != 0 {
		return NewErrorWithMetadata(errors.New("failed to purchase item"), res)
	}

	return nil
}

func (t *appstore) purchaseRequest(input PurchaseInput, storeFront, guid string, pricingParameters string) http.Request {
	acc := input.Account
	podPrefix := ""
	if acc.Pod != "" {
		podPrefix = "p" + acc.Pod + "-"
	}

	// ---------------------------------------------------
	// 🤖 核心兼容邏輯 (Fallback)
	// ---------------------------------------------------

	// 1. 決定目標 ID：如果有傳入內購 ID 就用內購的，否則退回使用 App.ID
	targetAdamId := fmt.Sprintf("%d", input.SalableAdamId)
	if input.SalableAdamId == 0 {
		targetAdamId = fmt.Sprintf("%d", input.App.ID)
	}

	// 2. 決定 AppExtVrsId：內購需要真實版本號，普通 App 允許傳 "0"
	appExtVrsId := input.AppExtVrsId
	if appExtVrsId == "" {
		appExtVrsId = "0"
	}

	// 3. 決定產品類型：預設為 "C" (Consumable/App)
	productType := input.ProductType
	if productType == "" {
		productType = "C"
	}

	// 4. 決定價格：優先使用傳入的 Price 字串，否則轉型舊的 App.Price
	price := input.Price
	if price == "" {
		if input.App.Price == 0 {
			price = "0"
		} else {
			price = fmt.Sprintf("%.2f", input.App.Price)
		}
	}

	// 5. 安全授權標籤：免費項目可以無授權購買，付費項目必須授權
	buyWithoutAuth := "false"
	if price == "0" || price == "0.00" {
		buyWithoutAuth = "true"
	}

	// 組合 Headers
	headers := map[string]string{
		"Content-Type":        "application/x-apple-plist",
		"iCloud-DSID":         acc.DirectoryServicesID,
		"X-Dsid":              acc.DirectoryServicesID,
		"X-Apple-Store-Front": storeFront,
		"X-Token":             acc.PasswordToken,
	}

	// 只有在外部有提供簽名時才塞入 (付費項目必填)
	if input.ActionSignature != "" {
		headers["X-Apple-ActionSignature"] = input.ActionSignature
	}

	// 組合 Payload
	payloadContent := map[string]interface{}{
		"appExtVrsId":               appExtVrsId,
		"hasAskedToFulfillPreorder": "true",
		"buyWithoutAuthorization":   buyWithoutAuth,
		"hasDoneAgeCheck":           "true",
		"guid":                      guid,
		"needDiv":                   "0",
		"origPage":                  fmt.Sprintf("Software-%d", input.App.ID),
		"origPageLocation":          "Buy",
		"price":                     price,
		"pricingParameters":         pricingParameters,
		"productType":               productType,
		"salableAdamId":             targetAdamId,
	}

	return http.Request{
		URL:            fmt.Sprintf("https://%s%s%s", podPrefix, PrivateAppStoreAPIDomain, PrivateAppStoreAPIPathPurchase),
		Method:         http.MethodPOST,
		ResponseFormat: http.ResponseFormatXML,
		Headers:        headers,
		Payload: &http.XMLPayload{
			Content: payloadContent,
		},
	}
}
