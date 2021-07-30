module Components.NodeGroup exposing (view)

import Api.Point as Point
import Components.NodeOptions exposing (NodeOptions)
import Element exposing (..)
import Element.Border as Border
import UI.Form as Form
import UI.Icon as Icon
import UI.Style exposing (colors)


view : NodeOptions msg -> Element msg
view o =
    let
        textInput =
            Form.nodeTextInput
                { onEditNodePoint = o.onEditNodePoint
                , node = o.node
                , now = o.now
                , labelWidth = 100
                }
                ""
                0
    in
    column
        [ width fill
        , Border.widthEach { top = 2, bottom = 0, left = 0, right = 0 }
        , Border.color colors.black
        , spacing 6
        ]
    <|
        wrappedRow [ spacing 10 ]
            [ Icon.users
            , text <|
                Point.getText o.node.points "" 0 Point.typeDescription
            ]
            :: (if o.expDetail then
                    [ textInput Point.typeDescription "Description"
                    ]

                else
                    []
               )
